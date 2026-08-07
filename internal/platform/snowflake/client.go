// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package snowflake is a READ-ONLY Snowflake client for the email channel. Its sole
// job is to resolve the exact past-edition EVENT_NAME strings a HubSpot
// BEHAVIORAL_EVENT audience filter needs, from
// ANALYTICS.PLATINUM_LFX_ONE.event_registrations. It exposes no arbitrary-SQL entry
// point: callers pass search terms and get back verified event names, so the query
// shape is fixed and parameterized (no SQL injection surface, no accidental writes).
//
// Auth is key-pair (JWT): the injected RSA private key signs the Snowflake JWT. All
// configuration is injected via NewClient; the package never reads the environment.
package snowflake

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"strings"
	"sync"
	"time"

	sf "github.com/snowflakedb/gosnowflake"
)

const (
	// defaultDatabase / defaultSchema / eventTable name the authoritative curated
	// source. Per the email-channel design (LFXV2-2770) the broker uses PLATINUM,
	// not the reference app's Silver_Segment.
	defaultDatabase = "ANALYTICS"
	defaultSchema   = "PLATINUM_LFX_ONE"
	eventTable      = "event_registrations"

	// maxEventRows caps how many distinct past editions a single resolve returns, so
	// an over-broad term can't pull an unbounded result into memory.
	maxEventRows = 500

	// queryTimeout bounds a single resolve query. A read against the curated table is
	// fast; this guards against a hung warehouse.
	queryTimeout = 30 * time.Second

	// escapeClause declares backslash as the ILIKE escape character (pairs with
	// likeContains). Snowflake parses the ESCAPE argument as a standard single-quoted
	// string literal in which backslash IS an escape character, so a single literal
	// backslash must be written as '\\' in the SQL text.
	escapeClause = `ESCAPE '\\'`

	// maxOpenConns caps concurrent Snowflake sessions the pool opens. This is a
	// low-QPS read path, so a small bound protects the warehouse from a session
	// storm under concurrent resolution (database/sql defaults to unlimited).
	maxOpenConns = 4

	// connMaxIdleTime releases idle sessions so the pool doesn't pin warehouse
	// sessions between infrequent resolves.
	connMaxIdleTime = 5 * time.Minute
)

// Config is the injected Snowflake connection configuration. PrivateKeyPEM is the
// unencrypted PKCS8 RSA private key in PEM form (the JWT signer). Warehouse/Role are
// optional.
//
// The query SOURCE (database/schema/table) is NOT configurable: event resolution
// always targets the authoritative ANALYTICS.PLATINUM_LFX_ONE.event_registrations via
// package constants, so a misconfigured caller can never silently resolve names from a
// different dataset. The DSN's session database/schema are set to the same constants.
type Config struct {
	Account       string
	User          string
	PrivateKeyPEM string
	Warehouse     string
	Role          string
}

// Client is a read-only Snowflake client. It holds a lazily-opened *sql.DB (a
// connection pool); it is safe for concurrent use.
//
// It does NOT retain the injected Config (which carries the PEM private key): after
// NewClient builds the DSN the PEM is dropped, so the credential isn't held in two
// places. The built DSN still embeds the signing key — that's unavoidable, since the
// gosnowflake driver needs it to open the pool — so the DSN itself is treated as
// secret (never logged or quoted into errors).
type Client struct {
	dsn    string
	opener func(dsn string) (*sql.DB, error) // injectable for tests

	mu     sync.Mutex // guards db + closed (lazy open + Close)
	db     *sql.DB
	closed bool
}

// Event is one resolved past-edition registration event.
type Event struct {
	EventName string
	EventID   string
}

// NewClient builds a read-only Snowflake client from injected config. It validates
// the config and parses the private key up front (a bad key is a deterministic
// config error), building the DSN, but does NOT connect — the pool opens lazily on
// the first query so an unreachable warehouse doesn't wedge construction.
func NewClient(cfg Config, opts ...Option) (*Client, error) {
	// Trim the identity fields once and use the TRIMMED values everywhere — otherwise
	// a whitespace-padded account/user passes the non-blank check but flows untrimmed
	// into the DSN, producing an invalid connection string.
	account := strings.TrimSpace(cfg.Account)
	user := strings.TrimSpace(cfg.User)
	if account == "" || user == "" {
		return nil, fmt.Errorf("snowflake: account and user are required")
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}

	// The session database/schema are pinned to the authoritative constants — the
	// same source the fully-qualified query targets — so they are never
	// caller-overridable.
	sfCfg := &sf.Config{
		Account:       account,
		User:          user,
		Database:      defaultDatabase,
		Schema:        defaultSchema,
		Warehouse:     strings.TrimSpace(cfg.Warehouse),
		Role:          strings.TrimSpace(cfg.Role),
		Authenticator: sf.AuthTypeJwt,
		PrivateKey:    key,
	}
	dsn, err := sf.DSN(sfCfg)
	if err != nil {
		// A DSN build failure would quote the config; keep it out of the message.
		return nil, fmt.Errorf("snowflake: build DSN failed (check account/user/warehouse)")
	}

	c := &Client{
		dsn: dsn,
		opener: func(dsn string) (*sql.DB, error) {
			return sql.Open("snowflake", dsn)
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Option customizes a Client (test seams).
type Option func(*Client)

// withOpener overrides the *sql.DB opener so tests can inject a fake (sqlmock).
func withOpener(o func(dsn string) (*sql.DB, error)) Option {
	return func(c *Client) { c.opener = o }
}

// pool lazily opens the *sql.DB (connection pool) on first use. Guarded by mu so
// concurrent first queries can't double-open (leaking a *sql.DB) or race Close.
func (c *Client) pool() (*sql.DB, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Refuse to (re)open after Close — otherwise a resolve that raced Close (or ran
	// after it) would silently open a FRESH pool that shutdown will never close,
	// leaking Snowflake sessions and defeating cleanup.
	if c.closed {
		return nil, fmt.Errorf("snowflake: client is closed")
	}
	if c.db != nil {
		return c.db, nil
	}
	db, err := c.opener(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("snowflake: open pool: %w", err)
	}
	// Bound the pool: database/sql defaults to UNLIMITED open connections, so under
	// concurrent audience resolution each query could open another Snowflake session
	// (the row limit and per-query timeout don't cap session count). This is a
	// low-QPS read path, so a small cap is plenty and protects the warehouse.
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	c.db = db
	return db, nil
}

// Close releases the connection pool. Guarded by mu (and nils the handle) so it
// can't race a concurrent lazy open or a second Close.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true // even if the pool was never opened, block a later lazy open
	if c.db != nil {
		err := c.db.Close()
		c.db = nil
		if err != nil {
			// Wrap with context (like the query/DSN errors) while preserving %w so a
			// shutdown failure stays diagnosable via errors.Is/errors.As.
			return fmt.Errorf("snowflake: close pool: %w", err)
		}
	}
	return nil
}

// ResolvePastEventNames returns the DISTINCT (EVENT_NAME, EVENT_ID) rows for past
// editions matching eventTerm (and, when non-empty, locationTerm), EXCLUDING the
// current-year edition. The returned EVENT_NAME strings are used VERBATIM as HubSpot
// BEHAVIORAL_EVENT filter values, so this is the single source of truth for them —
// callers must NOT substitute guessed/remembered names (fail-closed on error/empty).
//
// The query is fully parameterized (no term is interpolated into SQL); each term is
// wrapped as a `%term%` ILIKE pattern with its metacharacters escaped (see
// likeContains) so a literal `%` or `_` in a term matches literally instead of acting
// as a wildcard. currentYear (a 4-digit 19xx/20xx year, e.g. "2026") is REQUIRED and excludes
// that edition — it is the guarantee that only PAST editions are returned, so a blank
// or malformed value is rejected rather than silently dropping the exclusion. A blank
// eventTerm is likewise rejected (it would match everything).
func (c *Client) ResolvePastEventNames(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]Event, error) {
	eventTerm = strings.TrimSpace(eventTerm)
	if eventTerm == "" {
		return nil, fmt.Errorf("snowflake: ResolvePastEventNames requires a non-empty event term")
	}
	// currentYear gates the "past editions only" contract. If it were optional, a
	// blank/malformed value would silently drop the NOT-ILIKE exclusion and let the
	// CURRENT edition through — the opposite of the method's guarantee. Require a
	// 4-digit 19xx/20xx year: the range is what keeps this comparable to the years
	// yearInName can extract (see isSupportedYear).
	currentYear = strings.TrimSpace(currentYear)
	if !isSupportedYear(currentYear) {
		return nil, fmt.Errorf("snowflake: ResolvePastEventNames requires currentYear as a 4-digit 19xx/20xx year (got %q)", currentYear)
	}

	db, err := c.pool()
	if err != nil {
		return nil, err
	}

	// Fully-qualified, read-only SELECT DISTINCT against the AUTHORITATIVE source
	// (package constants, never caller-controlled). Only bind parameters vary; LIMIT
	// bounds the result. escapeClause pairs with likeContains's metacharacter escaping.
	//
	// NOTE: The query fetches ALL matching events (without year filtering in the SQL) because
	// event names embed years in various formats and positions. The year predicate is applied
	// in Go code (after fetching) to compare extracted years numerically against currentYear,
	// ensuring only truly past editions (year < currentYear) are returned, not editions from
	// currentYear onward that merely lack the current year string in their name.
	q := fmt.Sprintf(`SELECT DISTINCT EVENT_NAME, EVENT_ID
FROM %s.%s.%s
WHERE EVENT_NAME ILIKE ? %s`, ident(defaultDatabase), ident(defaultSchema), ident(eventTable), escapeClause)
	args := []any{likeContains(eventTerm)}
	if locationTerm = strings.TrimSpace(locationTerm); locationTerm != "" {
		q += "\n  AND EVENT_NAME ILIKE ? " + escapeClause
		args = append(args, likeContains(locationTerm))
	}
	// Fetch MORE than the cap to account for rows we'll filter out by year. We'll trim to
	// maxEventRows after filtering, so an over-broad term that yields many non-past editions
	// is still detected and refused.
	q += fmt.Sprintf("\nORDER BY EVENT_NAME\nLIMIT %d", (maxEventRows+1)*2)

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	rows, err := db.QueryContext(qctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("snowflake: query past events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Parse currentYear once for filtering below. We already validated it's a 4-digit
	// 19xx/20xx year, which is the same range yearInName can extract — so the comparison
	// below is between two values drawn from one range.
	currentYearInt, _ := parseYear(currentYear)

	const rawLimit = (maxEventRows + 1) * 2

	var out []Event
	var rawFetched int
	for rows.Next() {
		rawFetched++
		var e Event
		var id sql.NullString
		if err := rows.Scan(&e.EventName, &id); err != nil {
			return nil, fmt.Errorf("snowflake: scan event row: %w", err)
		}
		e.EventID = id.String

		// BOUND PAST EDITIONS BY YEAR: only include events with a year strictly BEFORE
		// currentYear. This ensures that when rebuilding a 2025 brief in 2026+, we do not
		// treat 2026/2027 editions as past. yearInName returns "" if no year is found;
		// a row without a recognizable year is not proven to predate year tracking — it can
		// also be a current or future edition whose name omits the year. Fail closed and exclude it.
		extractedYear := yearInName(e.EventName)
		if extractedYear == "" {
			// Yearless names are ambiguous; exclude them to prevent inclusion of unwanted editions.
			continue
		}
		extractedYearInt, _ := parseYear(extractedYear)
		if extractedYearInt >= currentYearInt {
			// This edition's year >= current year, so it is not past; skip it.
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snowflake: iterate event rows: %w", err)
	}
	// The raw SQL LIMIT is applied BEFORE year filtering (see the query comment above), and
	// ORDER BY EVENT_NAME is alphabetical, not chronological — so hitting the raw limit does
	// NOT mean we've seen every past edition; a past edition could sort alphabetically after
	// the cutoff and never have been fetched at all. Fail closed rather than silently return
	// a truncated (incomplete) audience: we cannot prove completeness once the raw fetch was
	// itself capped.
	if rawFetched >= rawLimit {
		return nil, fmt.Errorf("snowflake: event term %q matched at least %d rows before year filtering; narrow the search term", eventTerm, rawLimit)
	}
	if len(out) > maxEventRows {
		// More than the cap matched (after year filtering): fail closed rather than return
		// a silently truncated (incomplete) audience.
		return nil, fmt.Errorf("snowflake: event term %q matched more than %d editions; narrow the search term", eventTerm, maxEventRows)
	}
	return out, nil
}

// parsePrivateKey decodes an unencrypted PKCS8 RSA private key from PEM. It tolerates
// the common .env copy/paste mangling the reference app handles — wrapping quotes and
// literal \n / \r\n escapes / CRLF line endings — since the key often arrives via an
// env-injected secret.
func parsePrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	pemStr = strings.TrimSpace(pemStr)
	if pemStr == "" {
		return nil, fmt.Errorf("snowflake: private key is required")
	}
	// Strip a single layer of wrapping quotes.
	if len(pemStr) >= 2 && (pemStr[0] == '"' || pemStr[0] == '\'') && pemStr[len(pemStr)-1] == pemStr[0] {
		pemStr = strings.TrimSpace(pemStr[1 : len(pemStr)-1])
	}
	// Normalize escaped and real line endings to real newlines.
	r := strings.NewReplacer("\\r\\n", "\n", "\\n", "\n", "\r\n", "\n", "\r", "\n")
	pemStr = r.Replace(pemStr)

	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("snowflake: private key is not valid PEM (expected an unencrypted PKCS8 BEGIN/END PRIVATE KEY block)")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("snowflake: parse PKCS8 private key: %w", err)
	}
	rsaKey, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("snowflake: private key is not an RSA key")
	}
	return rsaKey, nil
}

// isSupportedYear reports whether s is a 4-digit year in the 19xx/20xx range.
//
// The range is not decoration, and it is deliberately the FULL two-byte prefix rather than a
// first-digit check (which would accept 1000-2999). yearInName can only ever EXTRACT a 19xx/20xx
// year from an event name, so a year outside that range is not comparable with the years it
// is compared AGAINST. Above the range a currentYear of "9999" leaves every real edition
// strictly below it and the exclusion never fires — "past editions only" quietly starts
// returning future ones; below it ("0202") every edition is excluded and the resolve returns
// nothing. The two predicates must be one, which is why the range lives here rather than at
// each comparison.
func isSupportedYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	if s[0:2] != "19" && s[0:2] != "20" {
		return false
	}
	return s[2] >= '0' && s[2] <= '9' && s[3] >= '0' && s[3] <= '9'
}

// likeContains builds a `%term%` ILIKE pattern that matches term as a LITERAL
// substring. It escapes the pattern metacharacters `\`, `%`, and `_` (backslash
// first, so it doesn't double-escape the ones it adds) to pair with the query's
// `escapeClause` (which emits `ESCAPE '\\'` — two backslashes in the SQL text, since
// Snowflake parses the ESCAPE literal by single-quoted-string rules where `\\` is one
// backslash). Without this, a term of `%` or `_` would act as a wildcard and match
// nearly every EVENT_NAME — the same "match everything" case the empty-term guard
// blocks.
func likeContains(term string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(term) + "%"
}

// ident guards a database/schema/table identifier: these are package constants
// today, but validate defensively so a future config-sourced value can never inject
// SQL. Only [A-Za-z0-9_] and a single dot-free segment are allowed.
func ident(s string) string {
	for _, r := range s {
		ok := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_'
		if !ok {
			// Fall back to a clearly-invalid identifier so the query errors loudly
			// rather than executing something unexpected.
			return "_invalid_identifier_"
		}
	}
	return s
}

// yearInName extracts a standalone 4-digit 19xx/20xx year from a string (e.g. an event name).
// Returns "" if no 4-digit year is found.
func yearInName(s string) string {
	for i := 0; i+4 <= len(s); i++ {
		c := s[i : i+4]
		if !isSupportedYear(c) {
			continue
		}
		// Reject a longer digit run (e.g. an id) that merely contains four digits.
		if (i == 0 || s[i-1] < '0' || s[i-1] > '9') && (i+4 == len(s) || s[i+4] < '0' || s[i+4] > '9') {
			return c
		}
	}
	return ""
}

// parseYear converts a 4-digit year string to an integer. Returns 0 if parsing fails.
// The caller should validate with isSupportedYear first.
func parseYear(s string) (int, error) {
	var y int
	_, err := fmt.Sscanf(s, "%d", &y)
	return y, err
}
