# 2026-08-24 — LFXV2-2643 a name is not a property

**Fix** — the source guard identified the DSN-bearing error by how it was SPELLED: an
identifier counted only if it was `err` or ended in `Err`/`Error`. A reviewer pointed out
that renaming the result to `failure` and passing it raw to `t.Fatalf` leaves the check
empty, and the mutation confirmed it — the guard stayed green while a credential-bearing
error was rendered unredacted.

This is the THIRD instance of one defect class on this guard, and the class is worth naming
because the first two fixes did not end it. Each version answered an easier question than
the one that matters:

- a fixed 5-line window asked "is a redactor NEAR this call" — a 4-line comment defeated it;
- scanning the whole `if` block still read one line at a time — a continuation-line argument
  defeated it;
- the AST fixed both of those, but still asked "is this identifier NAMED like an error" — a
  rename defeated it.

Line proximity approximated "is this call redacted". A name heuristic approximated "which
value is the error". Both are proxies, and a proxy holds only until someone writes the shape
nobody had written yet. `symbol-name-is-a-claim` states the general form: a name is a claim
about a value, not a property of it, and a claim has to be checked against the thing itself.

The guard now binds the identifier from the ASSIGNMENT that produced the error — the last
result of the DSN-bearing call, in either the plain `x, err := call()` form or an
`if x, err := call(); err != nil` initializer — and checks occurrences of THAT identifier.
The AST already knew this; the earlier version simply did not ask. Spelling is now irrelevant
by construction, which is the property that makes it not a fourth guess. A site binding `_`
contributes nothing to check, and is skipped rather than reported.

Fourteen compiling mutations pass over it now, including four the previous version could not
see: a renamed result passed raw, a rename at a different site through `Fatalf`, a rename
wrapped in `fmt.Sprintf`, and a rename in a mixed safe/raw call.

What has NOT changed, and should be read every time this guard is cited: it is SOURCE
INSPECTION. It proves the four sites SPELL a redactor around the value the call bound; it
does not prove the redactor's output is credential-free at runtime — the behavioural
`SafeDSNErr` tests pin that separately. It cannot follow an error through a helper defined
in another function, nor through a variable assigned from a redactor several statements
earlier. Its growing sophistication makes it LOOK like behavioural coverage, and that
appearance is the main risk it now carries.

Ref: LFXV2-2643
