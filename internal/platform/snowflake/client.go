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
)

// Config is the injected Snowflake connection configuration. PrivateKeyPEM is the
// unencrypted PKCS8 RSA private key in PEM form (the JWT signer). Warehouse/Role are
// optional. Database/Schema default to the PLATINUM source when empty.
type Config struct {
	Account       string
	User          string
	PrivateKeyPEM string
	Warehouse     string
	Role          string
	Database      string
	Schema        string
}

// Client is a read-only Snowflake client. It holds a lazily-opened *sql.DB (a
// connection pool); it is safe for concurrent use.
type Client struct {
	cfg    Config
	db     *sql.DB
	dsn    string
	opener func(dsn string) (*sql.DB, error) // injectable for tests
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
	if strings.TrimSpace(cfg.Account) == "" || strings.TrimSpace(cfg.User) == "" {
		return nil, fmt.Errorf("snowflake: account and user are required")
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	if cfg.Database == "" {
		cfg.Database = defaultDatabase
	}
	if cfg.Schema == "" {
		cfg.Schema = defaultSchema
	}

	sfCfg := &sf.Config{
		Account:       cfg.Account,
		User:          cfg.User,
		Database:      cfg.Database,
		Schema:        cfg.Schema,
		Warehouse:     cfg.Warehouse,
		Role:          cfg.Role,
		Authenticator: sf.AuthTypeJwt,
		PrivateKey:    key,
	}
	dsn, err := sf.DSN(sfCfg)
	if err != nil {
		// A DSN build failure would quote the config; keep it out of the message.
		return nil, fmt.Errorf("snowflake: build DSN failed (check account/user/warehouse)")
	}

	c := &Client{
		cfg: cfg,
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

// pool lazily opens the *sql.DB (connection pool) on first use.
func (c *Client) pool() (*sql.DB, error) {
	if c.db != nil {
		return c.db, nil
	}
	db, err := c.opener(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("snowflake: open pool: %w", err)
	}
	c.db = db
	return db, nil
}

// Close releases the connection pool.
func (c *Client) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

// ResolvePastEventNames returns the DISTINCT (EVENT_NAME, EVENT_ID) rows for past
// editions matching eventTerm (and, when non-empty, locationTerm), EXCLUDING the
// current-year edition. The returned EVENT_NAME strings are used VERBATIM as HubSpot
// BEHAVIORAL_EVENT filter values, so this is the single source of truth for them —
// callers must NOT substitute guessed/remembered names (fail-closed on error/empty).
//
// The query is fully parameterized (no term is interpolated into SQL); ILIKE
// wildcards are added around the bind values. currentYear excludes that edition
// (e.g. "2026"). A blank eventTerm is rejected (it would match everything).
func (c *Client) ResolvePastEventNames(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]Event, error) {
	eventTerm = strings.TrimSpace(eventTerm)
	if eventTerm == "" {
		return nil, fmt.Errorf("snowflake: ResolvePastEventNames requires a non-empty event term")
	}

	db, err := c.pool()
	if err != nil {
		return nil, err
	}

	// Fully-qualified, read-only SELECT DISTINCT. Only bind parameters vary; the
	// identifiers are constants (never caller-controlled). LIMIT bounds the result.
	q := fmt.Sprintf(`SELECT DISTINCT EVENT_NAME, EVENT_ID
FROM %s.%s.%s
WHERE EVENT_NAME ILIKE ?`, ident(c.cfg.Database), ident(c.cfg.Schema), ident(eventTable))
	args := []any{"%" + eventTerm + "%"}
	if locationTerm = strings.TrimSpace(locationTerm); locationTerm != "" {
		q += "\n  AND EVENT_NAME ILIKE ?"
		args = append(args, "%"+locationTerm+"%")
	}
	if currentYear = strings.TrimSpace(currentYear); currentYear != "" {
		q += "\n  AND EVENT_NAME NOT ILIKE ?"
		args = append(args, "%"+currentYear+"%")
	}
	q += fmt.Sprintf("\nORDER BY EVENT_NAME\nLIMIT %d", maxEventRows)

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	rows, err := db.QueryContext(qctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("snowflake: query past events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		var e Event
		var id sql.NullString
		if err := rows.Scan(&e.EventName, &id); err != nil {
			return nil, fmt.Errorf("snowflake: scan event row: %w", err)
		}
		e.EventID = id.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snowflake: iterate event rows: %w", err)
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
