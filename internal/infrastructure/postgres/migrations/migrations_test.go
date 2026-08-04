// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package migrations

import (
	"regexp"
	"strconv"
	"testing"
)

// migrationFileRE matches golang-migrate's required filename form:
// {version}_{name}.{up|down}.sql
var migrationFileRE = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$`)

// migrationSet is the parsed view of the embedded migration files, keyed by version.
type migrationSet struct {
	names map[uint64]string   // version -> migration name
	dirs  map[uint64][]string // version -> the directions present ("up"/"down")
}

func loadMigrations(t *testing.T) *migrationSet {
	t.Helper()
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	set := &migrationSet{names: map[uint64]string{}, dirs: map[uint64][]string{}}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		m := migrationFileRE.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("migration file %q does not match golang-migrate's {version}_{name}.{up|down}.sql form; it would be IGNORED by the iofs source driver and silently never applied", name)
			continue
		}
		version, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			t.Errorf("migration %q has an unparseable version %q: %v", name, m[1], err)
			continue
		}
		if prev, ok := set.names[version]; ok && prev != m[2] {
			// THE failure mode this test exists for. golang-migrate keys applied
			// state by version number alone: when two DIFFERENT migrations share a
			// version it records the version as applied after running one of them and
			// silently SKIPS the other. The table never exists, in any environment,
			// with no error anywhere.
			t.Errorf("version %d is claimed by two different migrations (%q and %q). golang-migrate applies ONE and silently skips the other — renumber one of them", version, prev, m[2])
		}
		set.names[version] = m[2]
		set.dirs[version] = append(set.dirs[version], m[3])
	}
	if len(set.names) == 0 {
		t.Fatal("no migrations found in the embedded FS; the //go:embed directive may have stopped matching")
	}
	return set
}

// TestMigrationVersionsAreUnique guards the numbering invariant that was previously
// prose-only (documented in the knowledge bundle but untested): no two migrations may
// claim the same version.
//
// That is the invariant with teeth. golang-migrate keys applied state by version
// number alone, so two migrations sharing a number means it runs one, records the
// version as applied, and silently SKIPS the other — the table never exists in any
// environment and nothing errors anywhere. The duplicate detection itself lives in
// loadMigrations (it needs the per-file parse); this test pins the resulting
// guarantee and the ordering property callers depend on.
//
// Contiguity is deliberately NOT asserted. A gap is normal and expected while
// migrations are in flight on unmerged branches: this branch holds 000011 while
// 000008-000010 are claimed by open PRs (see
// TestMigrationNumberingGapIsDocumented). A contiguity assertion would fail on every
// such branch, and a test that fails when nothing is wrong gets suppressed — taking
// the uniqueness check that DOES matter down with it.
func TestMigrationVersionsAreUnique(t *testing.T) {
	set := loadMigrations(t)

	// Versions must be strictly positive: golang-migrate treats version 0 as "no
	// migrations applied", so a 000000 migration could never be recorded as done.
	for v, name := range set.names {
		if v == 0 {
			t.Errorf("migration %q uses version 0, which golang-migrate reserves for the pre-migration state", name)
		}
	}

	// Every version must carry exactly one migration NAME. loadMigrations reports a
	// name collision; this asserts the set it produced is self-consistent.
	if len(set.names) != len(set.dirs) {
		t.Errorf("parsed %d migration names but %d version->direction entries; the two views disagree", len(set.names), len(set.dirs))
	}
}

// TestMigrationsHaveUpAndDown ensures every migration is reversible. A missing
// .down.sql makes a rollback impossible at exactly the moment it is most needed.
func TestMigrationsHaveUpAndDown(t *testing.T) {
	set := loadMigrations(t)

	for version, dirs := range set.dirs {
		var hasUp, hasDown bool
		for _, d := range dirs {
			switch d {
			case "up":
				if hasUp {
					t.Errorf("version %d (%s) has more than one .up.sql", version, set.names[version])
				}
				hasUp = true
			case "down":
				if hasDown {
					t.Errorf("version %d (%s) has more than one .down.sql", version, set.names[version])
				}
				hasDown = true
			}
		}
		if !hasUp {
			t.Errorf("version %d (%s) has no .up.sql", version, set.names[version])
		}
		if !hasDown {
			t.Errorf("version %d (%s) has no .down.sql — the migration cannot be rolled back", version, set.names[version])
		}
	}
}

// TestMigrationsAreNonEmpty catches a file that was created but never filled in: it
// would apply "successfully", mark its version done, and leave the schema unchanged.
func TestMigrationsAreNonEmpty(t *testing.T) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !migrationFileRE.MatchString(e.Name()) {
			continue
		}
		b, err := FS.ReadFile(e.Name())
		if err != nil {
			t.Errorf("read %s: %v", e.Name(), err)
			continue
		}
		// Every migration carries a 2-line licence header; anything at or below that
		// is a stub with no actual SQL.
		const licenceHeaderBytes = 80
		if len(b) <= licenceHeaderBytes {
			t.Errorf("%s is %d bytes — it looks like an empty stub. It would mark its version applied while changing nothing", e.Name(), len(b))
		}
	}
}

// TestMigrationVersionsMatchExpectedSet pins the CURRENT set so that adding a
// migration is a deliberate, reviewed act. When you add one, extend this list.
func TestMigrationVersionsMatchExpectedSet(t *testing.T) {
	set := loadMigrations(t)

	want := map[uint64]string{
		1:  "create_connection_tables",
		2:  "create_brief_campaign_tables",
		3:  "brief_partial_unique_slug",
		4:  "campaign_jobs_recovery_index",
		5:  "create_campaign_audiences_table",
		6:  "campaign_audiences_built_check",
		7:  "campaign_audiences_tenant_fk",
		11: "create_campaign_metrics_table",
	}

	for v, name := range want {
		got, ok := set.names[v]
		if !ok {
			t.Errorf("expected migration %d (%s) is absent", v, name)
			continue
		}
		if got != name {
			t.Errorf("migration %d is %q, expected %q", v, got, name)
		}
	}
	for v, name := range set.names {
		if _, ok := want[v]; !ok {
			t.Errorf("unexpected migration %d (%s): add it to the expected set in this test so the numbering stays reviewed", v, name)
		}
	}
}

// TestMigrationNumberingGapIsDocumented explains the deliberate 8..10 gap so the
// sequential test above does not read as broken to the next contributor.
func TestMigrationNumberingGapIsDocumented(t *testing.T) {
	set := loadMigrations(t)

	// Versions 8, 9 and 10 are claimed by IN-FLIGHT pull requests that are not yet
	// merged to main (#59 took 000008+000009; #60 took 000010). This branch
	// deliberately took 000011 to avoid a collision, which golang-migrate resolves
	// by silently skipping one migration.
	//
	// Once those PRs merge the gap closes on its own. Until then a hole is EXPECTED
	// here, so TestMigrationVersionsAreUniqueAndSequential is not run against a
	// contiguity requirement that main cannot yet satisfy.
	for _, v := range []uint64{8, 9, 10} {
		if name, ok := set.names[v]; ok {
			t.Logf("version %d (%s) is now present — the in-flight PR merged; the 8..10 gap has closed", v, name)
		}
	}
	if _, ok := set.names[11]; !ok {
		t.Error("migration 000011 (campaign metrics) is missing")
	}
}
