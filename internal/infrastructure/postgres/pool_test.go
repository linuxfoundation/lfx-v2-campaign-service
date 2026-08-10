// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestPgxURL_RewritesURLSchemes(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@host:5432/db":   "pgx5://u:p@host:5432/db",
		"postgresql://u:p@host:5432/db": "pgx5://u:p@host:5432/db",
	}
	for in, want := range cases {
		got, err := pgxURL(in)
		if err != nil {
			t.Errorf("pgxURL(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("pgxURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPgxURL_RejectsKeywordDSN(t *testing.T) {
	// golang-migrate cannot consume a keyword DSN, so Migrate must reject it
	// rather than pass it through and fail obscurely at driver selection.
	if _, err := pgxURL("host=localhost user=u dbname=d"); err == nil {
		t.Error("pgxURL(keyword DSN) = nil error, want a clear rejection")
	}
}

func TestPgxURL_RejectsRawPgx5DSN(t *testing.T) {
	// "pgx5://" is golang-migrate's INTERNAL scheme, produced only by pgxURL's own
	// translation. NewPool opens the same DATABASE_URL via pgxpool.ParseConfig,
	// which cannot parse "pgx5://" — so accepting a raw "pgx5://" input would let it
	// clear ValidateMigrationDSN and then 503-loop forever on every pool open. It
	// must be rejected up front as a deterministic config error.
	if _, err := pgxURL("pgx5://u:p@host:5432/db"); err == nil {
		t.Error("pgxURL(pgx5:// DSN) = nil error, want a clear rejection")
	}
}

func TestValidateMigrationDSN(t *testing.T) {
	// Valid URL DSNs pass (no connection is attempted).
	for _, ok := range []string{"postgres://app@host:5432/db?sslmode=disable", "postgresql://u:p@h/d"} {
		if err := ValidateMigrationDSN(ok); err != nil {
			t.Errorf("ValidateMigrationDSN(%q) = %v, want nil", ok, err)
		}
	}
	// A keyword DSN (no URL scheme), a syntactically MALFORMED URL, and a raw
	// "pgx5://" DSN (migrate's internal scheme, which pgxpool can't open — see
	// TestPgxURL_RejectsRawPgx5DSN) all fail up front rather than 503-looping:
	// the malformed one passes the prefix check but must be caught by the
	// parseability check, not deferred to NewPool/Migrate.
	for _, bad := range []string{"host=localhost user=u dbname=d", "postgres://[bad", "not a dsn at all", "pgx5://u@h/d"} {
		if err := ValidateMigrationDSN(bad); err == nil {
			t.Errorf("ValidateMigrationDSN(%q) = nil, want an error", bad)
		}
	}
}

// A malformed credential-bearing DATABASE_URL must NOT surface the password (or the
// raw DSN) in the returned error — NewContainer propagates it and main logs it. pgx's
// own ParseConfigError redacts the password, but we don't depend on that: the error
// message is DSN-free (dsnParseError), while the parse cause stays reachable via
// errors.Unwrap for diagnostics.
func TestValidateMigrationDSN_ErrorDoesNotLeakSecret(t *testing.T) {
	const secret = "SUPERSECRETpw"
	// A URL-form DSN that carries a password but fails to parse (bad port).
	dsn := "postgres://user:" + secret + "@host:notaport/db"
	err := ValidateMigrationDSN(dsn)
	if err == nil {
		t.Fatal("expected an error for a malformed DSN")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "notaport") || strings.Contains(err.Error(), "user:") {
		t.Errorf("error message leaked DSN material: %q", err.Error())
	}
	// The underlying pgx parse error must still be reachable for diagnostics.
	if errors.Unwrap(err) == nil {
		t.Error("the parse cause should remain reachable via errors.Unwrap")
	}
}

func TestIsPermanentMigrationErr(t *testing.T) {
	// A dirty schema is permanent — retrying can never clear it (needs a manual force).
	dirty := fmt.Errorf("apply migrations: %w", migrate.ErrDirty{Version: 3})
	if !IsPermanentMigrationErr(dirty) {
		t.Error("a wrapped migrate.ErrDirty must be classified permanent")
	}
	// Connectivity / deadline / generic errors are NOT permanent (they should retry).
	for _, transient := range []error{
		errors.New("dial tcp: connection refused"),
		context.DeadlineExceeded,
		fmt.Errorf("open database pool: %w", errors.New("ping database: timeout")),
		nil,
	} {
		if IsPermanentMigrationErr(transient) {
			t.Errorf("a non-dirty error must NOT be permanent: %v", transient)
		}
	}
}

func withSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

func TestCheckReady_SuccessRecordsOKSpan(t *testing.T) {
	sr := withSpanRecorder(t)
	p := &Pool{}

	ok := p.checkReady(context.Background(), func(context.Context) error { return nil })
	require.True(t, ok)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "postgres.ready", spans[0].Name())
	assert.Equal(t, codes.Ok, spans[0].Status().Code)
}

func TestCheckReady_FailureRecordsErrorSpan(t *testing.T) {
	sr := withSpanRecorder(t)
	p := &Pool{}

	ok := p.checkReady(context.Background(), func(context.Context) error {
		return errors.New("boom")
	})
	require.False(t, ok)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "postgres.ready", spans[0].Name())
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	require.NotEmpty(t, spans[0].Events(), "expected RecordError event")
}

func TestCheckReady_PassesContextToPing(t *testing.T) {
	_ = withSpanRecorder(t)
	p := &Pool{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sawCanceled bool
	ok := p.checkReady(ctx, func(c context.Context) error {
		sawCanceled = c.Err() != nil
		return c.Err()
	})
	require.False(t, ok)
	assert.True(t, sawCanceled)
}

// migrationIndexOwners is what the invalid-index remedy turns into "force N", so a wrong
// answer is worse than no answer: it replays unrelated DDL, or — the case this replaced —
// tells an operator to DROP an index a migration owns and leave the version alone.
//
// The parser is pinned against the real embedded migrations rather than a fixture, because
// the thing that can break it is the SQL this repo actually writes: a CONCURRENTLY, an
// IF NOT EXISTS, a quoted name, a UNIQUE. Every index the migrations create must be found.
func TestMigrationIndexOwners_FindsEveryCreatedIndex(t *testing.T) {
	entries, err := fs.Glob(migrations.FS, "*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	owners := migrationIndexOwners()

	// Independent of createIndexRe: find the names a different way (the token following
	// "INDEX", after the optional modifiers) and require the map to know each one. A regex
	// checked against itself proves nothing.
	for _, e := range entries {
		body, rerr := migrations.FS.ReadFile(e)
		require.NoError(t, rerr)
		for _, line := range strings.Split(string(body), "\n") {
			u := strings.ToUpper(strings.TrimSpace(line))
			if !strings.HasPrefix(u, "CREATE INDEX") && !strings.HasPrefix(u, "CREATE UNIQUE INDEX") {
				continue
			}
			fields := strings.Fields(strings.TrimSpace(line))
			var name string
			for i, f := range fields {
				if strings.EqualFold(f, "INDEX") {
					rest := fields[i+1:]
					for len(rest) > 0 && (strings.EqualFold(rest[0], "CONCURRENTLY") ||
						strings.EqualFold(rest[0], "IF") || strings.EqualFold(rest[0], "NOT") ||
						strings.EqualFold(rest[0], "EXISTS")) {
						rest = rest[1:]
					}
					if len(rest) > 0 {
						name = strings.Trim(rest[0], `"`)
					}
					break
				}
			}
			if name == "" {
				continue
			}
			v, ok := owners[name]
			assert.Truef(t, ok, "%s creates index %q but migrationIndexOwners does not know it; "+
				"an invalid copy would be reported as owned by no migration and an operator told "+
				"to drop it permanently", e, name)
			if ok {
				want, cerr := strconv.Atoi(strings.SplitN(e, "_", 2)[0])
				require.NoError(t, cerr)
				assert.GreaterOrEqualf(t, v, want,
					"index %q is created by %s but attributed to migration %06d", name, e, v)
			}
		}
	}
}

// TestRequiredIndexes_CoversTheDispatchUniqueIndex pins the membership, not the message.
//
// uq_campaigns_brief_platform_live is the sole arbiter of (brief_id, platform) uniqueness
// once 000014 drops campaigns_brief_id_platform_key, and 000014's guard checks it exactly
// once — while 000014 runs. An index lost any time afterwards left a schema that booted
// clean with concurrent claims free to double-create paid campaigns. Membership in
// requiredIndexes is what makes every later boot re-check it, and the DEFINITION is
// checked rather than the name because 000013 creates it with IF NOT EXISTS: any index
// carrying the name makes that a no-op, and a name-only check then calls the schema
// healthy (TestMigration000014_GuardChecksIndexDefinition records the PostgreSQL 16.10 run
// where a same-named NON-unique index passed the name-only form of 000014's own guard).
func TestRequiredIndexes_CoversTheDispatchUniqueIndex(t *testing.T) {
	const dispatchUnique = "uq_campaigns_brief_platform_live"

	var got *requiredIndex
	for i, ri := range requiredIndexes {
		if ri.name == dispatchUnique {
			got = &requiredIndexes[i]
		}
	}
	require.NotNilf(t, got, "%s is not in requiredIndexes: nothing re-checks the only "+
		"index enforcing dispatch uniqueness after 000014 drops the constraint", dispatchUnique)

	assert.Equal(t, "campaigns", got.table)
	assert.True(t, got.unique, "a non-unique index of this name arbitrates nothing")
	assert.Equal(t, []string{"brief_id", "platform"}, got.keys)
	// Character-identical to the deparsed form 000014's guard compares against. Two
	// checks on one definition are only two checks while they agree on what it is.
	assert.Equal(t, "(status <> 'deleted'::text)", got.predicate)
}

// describeInvalid's annotation is derived from the migrations, not from requiredIndexes,
// and that has to keep holding for indexes the registry does not name — otherwise an
// operator holding an invalid copy of one is told to drop it and leave the schema version
// alone, which removes it permanently. idx_campaigns_stuck_claims is such an index: 000008
// creates it, nothing re-checks it at boot, and the stuck-claim scan degrades to a full
// table scan without it.
func TestDescribeInvalid_AnnotatesIndexesOutsideRequiredIndexes(t *testing.T) {
	const stuck = "idx_campaigns_stuck_claims"
	require.NotContains(t,
		func() []string {
			var n []string
			for _, ri := range requiredIndexes {
				n = append(n, ri.name)
			}
			return n
		}(), stuck,
		"this test is only meaningful while the index is outside requiredIndexes")

	got := describeInvalid([]string{stuck})
	assert.Containsf(t, got, stuck+" (migration 000008: force 7)",
		"describeInvalid = %q, want the index annotated with the migration that "+
			"unconditionally creates it", got)
	assert.NotContains(t, got, "no migration creates this")
}

// The scan is schema-wide, so it does also turn up names no migration owns — a hand-built
// index, an operator's experiment. Those must NOT carry force advice: the operator would
// replay unrelated DDL to fix an index nothing will recreate. The live test builds exactly
// such an index by hand.
func TestDescribeInvalid_LeavesTrulyUnownedIndexesAlone(t *testing.T) {
	owned := requiredIndexes[0].name
	got := describeInvalid([]string{owned, "zz_hand_built_idx"})

	assert.Containsf(t, got, owned+" (migration 000018: force 17)",
		"describeInvalid = %q, want the owned index annotated with its version", got)
	assert.Containsf(t, got, "zz_hand_built_idx (no migration creates this",
		"describeInvalid = %q, want the unowned index told to leave the version alone", got)
}

// TestRequiredIndexes_AnnotateToTheMigrationThatRebuildsThem pins the reason the
// missing-index error annotates per NAME instead of ending in one
// `migrate force <version-1>` sentence.
//
// The generic sentence was correct while requiredIndexes held a single entry and became
// wrong the moment it held two, because the two are created by different migrations. An
// operator holding a schema missing uq_campaigns_brief_platform_live who forces 17 replays
// 000018, rebuilds the AUDIENCE index, and boots against a campaigns table that still has
// nothing enforcing (brief_id, platform) uniqueness — having followed the message exactly.
//
// An earlier form of this test asserted the annotations were all DISTINCT, as a canary for
// that hazard. That was the wrong invariant, and the seven connection indexes are the case
// that shows why: all seven are created by 000001, so all seven annotate to "force 0" — and
// forcing 0 really does replay 000001 and rebuild every one of them. Shared owners are not
// the danger; they are the case the per-name annotation handles trivially. The danger is an
// annotation that does NOT rebuild the index it is attached to, which is what this asserts:
// every entry resolves to a migration, and that migration's CREATE really carries its name.
func TestRequiredIndexes_AnnotateToTheMigrationThatRebuildsThem(t *testing.T) {
	owners := migrationIndexOwners()
	require.NotEmpty(t, requiredIndexes)

	for _, idx := range requiredIndexes {
		got := indexRecovery(idx.name)
		require.NotContainsf(t, got, "no migration creates this",
			"%s is in requiredIndexes but no migration creates it: the error would tell the "+
				"operator to drop an index the service refuses to boot without", idx.name)

		// The annotation is only advice until the named migration is confirmed to contain
		// a CREATE for THIS index. Deriving a name by convention — as the seven connection
		// entries do — is exactly how an entry acquires a plausible name nothing creates.
		version, ok := owners[idx.name]
		assert.Truef(t, ok, "%s resolves to no owning migration", idx.name)
		if !ok {
			continue
		}
		assert.Containsf(t, got, fmt.Sprintf("force %d", version-1),
			"%s annotates to %q, but its CREATE is in migration %06d: forcing any other "+
				"version replays a migration that does not rebuild it", idx.name, got, version)
	}
}

// TestMigrationIndexOwners_IgnoresAConditionalRebuild is the finding that a CREATE inside a
// DO block cannot be a recovery target.
//
// The remedy the map feeds is "DROP the index, then force back so the migration RUNS again".
// 000009 rebuilds idx_campaigns_stuck_claims only IF an INVALID copy is present — and the
// operator's drop, the first half of that remedy, is precisely what makes the condition
// false. Attributing the index to 000009 would send them to force 8, watch 000009 no-op, and
// boot clean with the index gone for good: the stuck-claim scan full-scanning forever, which
// is the failure 000008 and 000009 exist to prevent. 000008's plain CREATE ... IF NOT EXISTS
// does fire against a schema missing the name, so it is the answer.
func TestMigrationIndexOwners_IgnoresAConditionalRebuild(t *testing.T) {
	const stuck = "idx_campaigns_stuck_claims"

	// The premise: 000009 really does contain a CREATE for this name, inside a DO block.
	body, err := migrations.FS.ReadFile("000009_drop_invalid_stuck_claim_index.up.sql")
	require.NoError(t, err)
	require.Contains(t, string(body), "CREATE INDEX "+stuck,
		"this test is only meaningful while 000009 rebuilds the index")

	assert.Equalf(t, 8, migrationIndexOwners()[stuck],
		"%s must be attributed to 000008, whose CREATE fires against a schema missing the "+
			"index; 000009's rebuild is conditional on the invalid copy the remedy deletes", stuck)
	assert.Contains(t, describeInvalid([]string{stuck}), stuck+" (migration 000008: force 7)")
}

// executableSQL is the reason the answer above is 8, and it strips two different things for
// two different reasons. The prose case is not hypothetical: 000009's header contains the
// phrase "a failed CREATE INDEX CONCURRENTLY", from which createIndexRe reads the index name
// "does". Inert today — nothing is called that — but a scan whose output depends on comment
// wording is one prose edit away from claiming a migration owns an index it never touches.
func TestExecutableSQL(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"line comment": {"-- CREATE INDEX a\nCREATE INDEX b;", "\nCREATE INDEX b;"},
		"dollar block": {"CREATE INDEX a;\nDO $$ CREATE INDEX b; $$;\n", "CREATE INDEX a;\nDO ;\n"},
		"tagged block": {"A $tag$ hidden $tag$ B", "A  B"},
		// Two blocks in a row: a tag-agnostic regexp would close the first on the second's
		// OPENING delimiter and swallow the statement between them.
		"two blocks":    {"$$x$$ CREATE INDEX keep; $$y$$", " CREATE INDEX keep; "},
		"unterminated":  {"CREATE INDEX a; DO $$ CREATE INDEX b;", "CREATE INDEX a; DO "},
		"no delimiters": {"CREATE INDEX a;", "CREATE INDEX a;"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, executableSQL(tc.in))
		})
	}

	// And the property that matters, stated against the real file rather than a fixture.
	body, err := migrations.FS.ReadFile("000009_drop_invalid_stuck_claim_index.up.sql")
	require.NoError(t, err)
	assert.NotContains(t, executableSQL(string(body)), "CREATE INDEX",
		"000009 executes no unconditional CREATE INDEX; every one it contains is prose or "+
			"inside the DO block")
}
