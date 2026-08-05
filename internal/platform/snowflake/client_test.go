// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package snowflake

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/pem"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// A tiny in-process database/sql driver fake (no external dependency). It records
// the last query + args and replays canned rows, so tests can assert the SQL shape
// and row handling without a live Snowflake.
// ---------------------------------------------------------------------------

type fakeDriver struct {
	mu    sync.Mutex
	query string
	args  []driver.Value
	rows  [][]driver.Value // canned result rows (EVENT_NAME, EVENT_ID)
	cols  []string
	qErr  error
}

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{d: d}, nil }

type fakeConn struct{ d *fakeDriver }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("no tx") }

// QueryContext lets us capture the query and args and return canned rows.
func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.query = query
	c.d.args = make([]driver.Value, len(args))
	for i, a := range args {
		c.d.args[i] = a.Value
	}
	if c.d.qErr != nil {
		return nil, c.d.qErr
	}
	return &fakeRows{cols: c.d.cols, data: c.d.rows}, nil
}

type fakeRows struct {
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// newFakeClient wires a Client whose opener returns a *sql.DB backed by drv.
func newFakeClient(t *testing.T, drv *fakeDriver) *Client {
	t.Helper()
	name := fmt.Sprintf("snowflake-fake-%p", drv)
	sql.Register(name, drv)
	c, err := NewClient(testConfig(t), withOpener(func(string) (*sql.DB, error) {
		return sql.Open(name, "ignored-dsn")
	}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// testConfig returns a valid config with a freshly-generated RSA key in PEM.
func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Account:       "acct",
		User:          "user",
		PrivateKeyPEM: genPKCS8PEM(t),
	}
}

func genPKCS8PEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestNewClient_ValidatesConfig(t *testing.T) {
	if _, err := NewClient(Config{User: "u", PrivateKeyPEM: genPKCS8PEM(t)}); err == nil {
		t.Error("missing account should error")
	}
	if _, err := NewClient(Config{Account: "a", PrivateKeyPEM: genPKCS8PEM(t)}); err == nil {
		t.Error("missing user should error")
	}
	if _, err := NewClient(Config{Account: "a", User: "u", PrivateKeyPEM: "not a key"}); err == nil {
		t.Error("bad private key should error")
	}
	// Whitespace-only account/user must be rejected (they'd trim to empty).
	if _, err := NewClient(Config{Account: "  ", User: "u", PrivateKeyPEM: genPKCS8PEM(t)}); err == nil {
		t.Error("whitespace-only account should error")
	}
	// A padded-but-valid account/user must be accepted (trimmed, not rejected, and
	// not flowed untrimmed into the DSN).
	if _, err := NewClient(Config{Account: "  acct  ", User: "  user  ", PrivateKeyPEM: genPKCS8PEM(t)}); err != nil {
		t.Errorf("a padded account/user should be trimmed and accepted: %v", err)
	}
}

func TestSource_IsAlwaysPlatinum(t *testing.T) {
	// The query source is not caller-configurable: it always targets the
	// authoritative PLATINUM constants, so a misconfigured caller can't resolve
	// names from a different dataset.
	if defaultDatabase != "ANALYTICS" || defaultSchema != "PLATINUM_LFX_ONE" {
		t.Errorf("source constants drifted: %s.%s", defaultDatabase, defaultSchema)
	}
	drv := &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}}
	c := newFakeClient(t, drv)
	if _, err := c.ResolvePastEventNames(context.Background(), "OSSNA", "", "2026"); err != nil {
		t.Fatalf("ResolvePastEventNames: %v", err)
	}
	if !strings.Contains(drv.query, "ANALYTICS.PLATINUM_LFX_ONE.event_registrations") {
		t.Errorf("query must target the authoritative PLATINUM source:\n%s", drv.query)
	}
}

func TestResolvePastEventNames_QueryShapeAndRows(t *testing.T) {
	drv := &fakeDriver{
		cols: []string{"EVENT_NAME", "EVENT_ID"},
		rows: [][]driver.Value{
			{"KubeCon + CloudNativeCon North America 2025", "ev-1"},
			{"KubeCon + CloudNativeCon North America 2024", "ev-2"},
			{"KubeCon + CloudNativeCon North America 2026", "ev-3"}, // Future edition, should be filtered
		},
	}
	c := newFakeClient(t, drv)

	got, err := c.ResolvePastEventNames(context.Background(), "KubeCon", "North America", "2026")
	if err != nil {
		t.Fatalf("ResolvePastEventNames: %v", err)
	}
	// 2026 edition should be filtered out because 2026 >= currentYear (2026)
	if len(got) != 2 || got[0].EventName != "KubeCon + CloudNativeCon North America 2025" || got[0].EventID != "ev-1" {
		t.Fatalf("rows = %+v (expected 2 past editions, currentYear editions filtered out)", got)
	}

	// The query must be a read-only, fully-qualified, parameterized SELECT DISTINCT
	// against the PLATINUM table. Year filtering is now done in code (comparing years
	// numerically) rather than in SQL, so the query does NOT contain "NOT ILIKE".
	// The LIMIT is doubled to account for rows we'll filter in code.
	q := drv.query
	for _, want := range []string{
		"SELECT DISTINCT EVENT_NAME, EVENT_ID",
		"ANALYTICS.PLATINUM_LFX_ONE.event_registrations",
		"EVENT_NAME ILIKE ?",
		"LIMIT 1002", // (maxEventRows+1)*2, to account for year filtering in code
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q\nquery:\n%s", want, q)
		}
	}
	// The SQL must NOT contain "NOT ILIKE" for year filtering anymore (it's in code).
	if strings.Contains(q, "NOT ILIKE") {
		t.Errorf("query must not contain SQL-level year filtering (now done in code):\n%s", q)
	}
	// No caller term is ever interpolated into the SQL text — only bind args.
	if strings.Contains(q, "KubeCon") || strings.Contains(q, "North America") || strings.Contains(q, "2026") {
		t.Errorf("query interpolated a caller term (SQL-injection risk):\n%s", q)
	}
	// The two ILIKE bind args carry the wildcards (no current-year exclusion in SQL anymore).
	wantArgs := []driver.Value{"%KubeCon%", "%North America%"}
	if len(drv.args) != 2 {
		t.Fatalf("args = %v, want 2 bind params (event term + location, year filtering in code)", drv.args)
	}
	for i, w := range wantArgs {
		if drv.args[i] != w {
			t.Errorf("arg[%d] = %v, want %v", i, drv.args[i], w)
		}
	}
}

func TestResolvePastEventNames_ExcludesYearlessNames(t *testing.T) {
	// A row whose event name carries no 4-digit year is ambiguous — it cannot be proven
	// to predate currentYear, so it must be excluded (fail closed) rather than included.
	drv := &fakeDriver{
		cols: []string{"EVENT_NAME", "EVENT_ID"},
		rows: [][]driver.Value{
			{"KubeCon + CloudNativeCon North America", "ev-yearless"},
			{"KubeCon + CloudNativeCon North America 2025", "ev-past"},
		},
	}
	c := newFakeClient(t, drv)

	got, err := c.ResolvePastEventNames(context.Background(), "KubeCon", "North America", "2026")
	if err != nil {
		t.Fatalf("ResolvePastEventNames: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "ev-past" {
		t.Fatalf("rows = %+v, want only the 2025 edition (yearless row excluded)", got)
	}
}

func TestResolvePastEventNames_EscapesLikeMetacharacters(t *testing.T) {
	drv := &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}}
	c := newFakeClient(t, drv)
	// A term containing ILIKE metacharacters must be escaped so it matches
	// literally, not as a wildcard (otherwise "%"/"_" match nearly everything —
	// the same "match everything" case the empty-term guard blocks).
	if _, err := c.ResolvePastEventNames(context.Background(), `50%_off\x`, "", "2026"); err != nil {
		t.Fatalf("ResolvePastEventNames: %v", err)
	}
	// backslash doubled, then % and _ escaped, wrapped in literal %…%. This is the
	// FIRST bind arg (the event term); the current-year exclusion binds after it.
	want := driver.Value(`%50\%\_off\\x%`)
	if len(drv.args) < 1 || drv.args[0] != want {
		t.Errorf("escaped bind arg[0] = %v, want %v", drv.args, want)
	}
	// The query must declare ESCAPE '\\' (two backslashes in the SQL text — Snowflake
	// parses the ESCAPE literal by string-literal rules, where \\ is one backslash).
	if !strings.Contains(drv.query, `ESCAPE '\\'`) {
		t.Errorf("query must declare ESCAPE '\\\\':\n%s", drv.query)
	}
}

func TestClient_ResolveAfterCloseFails(t *testing.T) {
	// After Close, a resolve must NOT silently open a fresh pool (which shutdown
	// would never close) — it must fail closed.
	drv := &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}}
	c := newFakeClient(t, drv)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.ResolvePastEventNames(context.Background(), "KubeCon", "", "2026"); err == nil {
		t.Error("a resolve after Close must fail, not re-open the pool")
	}
	// Close before first use must also block a later open.
	c2 := newFakeClient(t, &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}})
	_ = c2.Close()
	if _, err := c2.pool(); err == nil {
		t.Error("pool() after a pre-use Close must fail, not open a fresh pool")
	}
}

func TestClient_ConcurrentFirstUse(t *testing.T) {
	// The lazy pool open must be race-free: many goroutines hitting the first query
	// at once must not double-open or race Close (exercised under `go test -race`).
	drv := &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}}
	c := newFakeClient(t, drv)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.ResolvePastEventNames(context.Background(), "KubeCon", "", "2026")
		}()
	}
	wg.Wait()
}

func TestResolvePastEventNames_OmitsOptionalLocation(t *testing.T) {
	drv := &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}}
	c := newFakeClient(t, drv)
	// location is optional; currentYear is required. With no location, exactly one bind
	// is sent: the event term. Year filtering is now done in code, not SQL.
	if _, err := c.ResolvePastEventNames(context.Background(), "OSSNA", "", "2026"); err != nil {
		t.Fatalf("ResolvePastEventNames: %v", err)
	}
	if len(drv.args) != 1 {
		t.Errorf("want 1 bind (event term only) with no location, year filtering in code; got %v", drv.args)
	}
	if strings.Contains(drv.query, "NOT ILIKE") {
		t.Error("SQL-level year filtering (NOT ILIKE) must not be present; year filtering is done in code")
	}
}

func TestResolvePastEventNames_FailsClosedOnTruncation(t *testing.T) {
	// The query fetches (maxEventRows+1)*2 rows; after year filtering in code, if more than
	// maxEventRows past editions match, the method must fail closed (not silently return a
	// truncated, incomplete audience).
	rows := make([][]driver.Value, (maxEventRows+1)*2)
	for i := range rows {
		// All events are from year 2000 (before currentYear 2026), so none will be filtered by year
		rows[i] = []driver.Value{fmt.Sprintf("Event 2000 %d", i), fmt.Sprintf("ev-%d", i)}
	}
	drv := &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}, rows: rows}
	c := newFakeClient(t, drv)
	_, err := c.ResolvePastEventNames(context.Background(), "Event", "", "2026")
	if err == nil || !strings.Contains(err.Error(), "narrow the search term") {
		t.Errorf("an over-broad term must fail closed, got: %v", err)
	}
}

func TestResolvePastEventNames_FailsClosedOnRawLimitEvenWhenFewRowsArePast(t *testing.T) {
	// The raw SQL LIMIT is hit before year filtering, and ORDER BY EVENT_NAME is alphabetical,
	// not chronological. So a raw fetch that hits the limit can still yield FEW past editions
	// after filtering (e.g. most matches are future editions that happen to sort first) — the
	// old post-filter-only check (len(out) > maxEventRows) would not catch this, and would
	// silently return an incomplete "success" even though a past edition sorting after the
	// truncated raw fetch was never seen at all.
	rows := make([][]driver.Value, (maxEventRows+1)*2)
	for i := range rows {
		if i == 0 {
			// The one past edition, sorted first alphabetically ("Event 2000" < "Event 2027").
			rows[i] = []driver.Value{"Event 2000 A", "ev-past"}
			continue
		}
		// All the rest are future editions (year >= currentYear 2026), filtered out.
		rows[i] = []driver.Value{fmt.Sprintf("Event 2027 %d", i), fmt.Sprintf("ev-future-%d", i)}
	}
	drv := &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}, rows: rows}
	c := newFakeClient(t, drv)
	_, err := c.ResolvePastEventNames(context.Background(), "Event", "", "2026")
	if err == nil || !strings.Contains(err.Error(), "narrow the search term") {
		t.Errorf("hitting the raw limit must fail closed regardless of how many rows survive year filtering, got: %v", err)
	}
}

func TestResolvePastEventNames_RequiresValidCurrentYear(t *testing.T) {
	c := newFakeClient(t, &fakeDriver{cols: []string{"EVENT_NAME", "EVENT_ID"}})
	// A blank or malformed currentYear must be rejected — otherwise the past-editions
	// guarantee silently breaks and the CURRENT edition could be returned.
	for _, bad := range []string{"", "  ", "26", "20260", "abcd", "202x"} {
		if _, err := c.ResolvePastEventNames(context.Background(), "KubeCon", "", bad); err == nil {
			t.Errorf("currentYear %q must be rejected (needs a 4-digit year)", bad)
		}
	}
}

func TestResolvePastEventNames_RejectsEmptyTerm(t *testing.T) {
	c := newFakeClient(t, &fakeDriver{})
	if _, err := c.ResolvePastEventNames(context.Background(), "  ", "x", "2026"); err == nil {
		t.Error("an empty event term must be rejected (it would match everything)")
	}
}

func TestResolvePastEventNames_QueryErrorPropagates(t *testing.T) {
	drv := &fakeDriver{qErr: fmt.Errorf("warehouse suspended")}
	c := newFakeClient(t, drv)
	_, err := c.ResolvePastEventNames(context.Background(), "KubeCon", "", "2026")
	if err == nil || !strings.Contains(err.Error(), "query past events") {
		t.Errorf("a query failure must propagate (fail-closed), got: %v", err)
	}
}

func TestParsePrivateKey_ToleratesEnvMangling(t *testing.T) {
	clean := genPKCS8PEM(t)
	// Simulate .env mangling: wrapping quotes + literal \n escapes.
	escaped := `"` + strings.ReplaceAll(strings.TrimSpace(clean), "\n", `\n`) + `"`
	if _, err := parsePrivateKey(escaped); err != nil {
		t.Errorf("parsePrivateKey should tolerate quoted + \\n-escaped PEM: %v", err)
	}
	// CRLF line endings.
	crlf := strings.ReplaceAll(clean, "\n", "\r\n")
	if _, err := parsePrivateKey(crlf); err != nil {
		t.Errorf("parsePrivateKey should tolerate CRLF: %v", err)
	}
	if _, err := parsePrivateKey(""); err == nil {
		t.Error("empty key should error")
	}
	if _, err := parsePrivateKey("-----BEGIN PRIVATE KEY-----\ngarbage\n-----END PRIVATE KEY-----"); err == nil { // secretlint-disable-line -- non-key fixture asserting a garbage body errors
		t.Error("garbage key should error")
	}
}

func TestParsePrivateKey_RejectsNonRSA(t *testing.T) {
	// A VALID PKCS8 key that is not RSA must be rejected at the type assertion (the
	// RSA-only authentication contract). Generate a real EC P-256 key, marshal it as
	// PKCS8, and assert rejection — so the non-RSA branch is actually exercised (a
	// malformed DER would fail earlier, at ParsePKCS8PrivateKey, and never reach it).
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen EC key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshal EC key: %v", err)
	}
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := parsePrivateKey(ecPEM); err == nil {
		t.Error("a valid non-RSA (EC) PKCS8 key must be rejected by the RSA-only contract")
	}
	// A malformed DER still errors (at the parse step) — keep that covered too.
	if _, err := parsePrivateKey("-----BEGIN PRIVATE KEY-----\nMEE=\n-----END PRIVATE KEY-----"); err == nil { // secretlint-disable-line -- non-key fixture asserting a malformed PKCS8 body errors
		t.Error("a malformed PKCS8 body must error")
	}
}

func TestIdent_RejectsInjection(t *testing.T) {
	if ident("event_registrations") != "event_registrations" {
		t.Error("a clean identifier must pass through")
	}
	for _, bad := range []string{"a; DROP TABLE x", "a.b", "a b", "a'--"} {
		if ident(bad) != "_invalid_identifier_" {
			t.Errorf("ident(%q) must be neutralized", bad)
		}
	}
}

// Ensure the driver value type is what database/sql expects (compile-time guard).
var _ driver.QueryerContext = (*fakeConn)(nil)
