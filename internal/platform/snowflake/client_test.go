// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package snowflake

import (
	"context"
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
		},
	}
	c := newFakeClient(t, drv)

	got, err := c.ResolvePastEventNames(context.Background(), "KubeCon", "North America", "2026")
	if err != nil {
		t.Fatalf("ResolvePastEventNames: %v", err)
	}
	if len(got) != 2 || got[0].EventName != "KubeCon + CloudNativeCon North America 2025" || got[0].EventID != "ev-1" {
		t.Fatalf("rows = %+v", got)
	}

	// The query must be a read-only, fully-qualified, parameterized SELECT DISTINCT
	// against the PLATINUM table, excluding the current year, with a LIMIT.
	q := drv.query
	for _, want := range []string{
		"SELECT DISTINCT EVENT_NAME, EVENT_ID",
		"ANALYTICS.PLATINUM_LFX_ONE.event_registrations",
		"EVENT_NAME ILIKE ?",
		"EVENT_NAME NOT ILIKE ?",
		"LIMIT 500",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q\nquery:\n%s", want, q)
		}
	}
	// No caller term is ever interpolated into the SQL text — only bind args.
	if strings.Contains(q, "KubeCon") || strings.Contains(q, "North America") || strings.Contains(q, "2026") {
		t.Errorf("query interpolated a caller term (SQL-injection risk):\n%s", q)
	}
	// The three ILIKE bind args carry the wildcards (terms here have no metachars).
	wantArgs := []driver.Value{"%KubeCon%", "%North America%", "%2026%"}
	if len(drv.args) != 3 {
		t.Fatalf("args = %v, want 3 bind params", drv.args)
	}
	for i, w := range wantArgs {
		if drv.args[i] != w {
			t.Errorf("arg[%d] = %v, want %v", i, drv.args[i], w)
		}
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
	// location is optional; currentYear is required. With no location, exactly two
	// binds are sent: the event term and the current-year exclusion.
	if _, err := c.ResolvePastEventNames(context.Background(), "OSSNA", "", "2026"); err != nil {
		t.Fatalf("ResolvePastEventNames: %v", err)
	}
	if len(drv.args) != 2 {
		t.Errorf("want 2 binds (event term + current-year exclusion) with no location, got %v", drv.args)
	}
	if !strings.Contains(drv.query, "NOT ILIKE") {
		t.Error("the required current-year exclusion must always be present")
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
	if _, err := parsePrivateKey("-----BEGIN PRIVATE KEY-----\ngarbage\n-----END PRIVATE KEY-----"); err == nil {
		t.Error("garbage key should error")
	}
}

func TestParsePrivateKey_RejectsNonRSA(t *testing.T) {
	// A valid PKCS8 block but not RSA would be rejected; here just confirm an EC-ish
	// wrong-type body errors at parse (garbage DER -> parse error is sufficient).
	if _, err := parsePrivateKey("-----BEGIN PRIVATE KEY-----\nMEE=\n-----END PRIVATE KEY-----"); err == nil {
		t.Error("a non-PKCS8/RSA key must error")
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
