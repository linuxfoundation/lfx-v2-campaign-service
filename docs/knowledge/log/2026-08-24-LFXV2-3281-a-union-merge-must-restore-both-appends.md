# 2026-08-24 — an append/append conflict is a union, and diff3 has a THIRD section

**Merge** — merging `origin/main` (which carried the conversions work, `d947dfb8`) into
`feat/LFXV2-3281-linkedin-refresh` conflicted in
`internal/platform/linkedin/metrics_test.go`. Both sides had appended new tests to the end of
the same file and neither had touched the other's lines, so the correct resolution keeps
everything from both sides.

## The mistake the first attempt made

The repository has `merge.conflictStyle` set to a diff3-family style, so a conflict region has
FOUR markers, not three:

```
<<<<<<< HEAD
(ours)
||||||| c90d8bf7
(the merge base)
=======
(theirs)
>>>>>>> origin/main
```

A resolver that greps only for `<<<<<<<`, `=======` and `>>>>>>>` does not see the
`|||||||` line. Slicing the file on those three positions therefore treats
`ours + ||||||| + base` as a single "ours" section: the base text is re-appended as if it
were new code, and the `|||||||` line itself survives into the output. `go build` did not
catch it, because the damage landed in a `_test.go` file that `go build` does not compile.
`go vet ./...` did, with `expected declaration, found '||'`.

**Both markers matter and so does the gate that runs on test files.** `go build ./...` is not
sufficient to declare a merge repaired when the conflict was in a test file.

## What the resolution actually needs

Not marker arithmetic — the three blobs:

```
git show <branch-head>:<path>   # ours
git show origin/main:<path>     # theirs
git show <merge-base>:<path>    # base
```

Here `ours` was `base` plus one import (`sync/atomic`) plus a 118-line append, and `theirs`
was `base` plus a 270-line append and nothing else. The union is base + the import + both
appends. The check that proves it is a set comparison of declared symbols, not a line count:

```
ours funcs: 42   theirs: 49   union: 53   merged: 53
MISSING from merged: []   EXTRA in merged: []
```

53, not 42+49=91, because the two sides share the 38 tests already in the base. A merged file
with 91 functions would mean the base tests had been duplicated — the same defect this repo
has already seen in `design/connection.go` (duplicate Goa methods) and in a docs index
(duplicate catalog entries). **Assert the symbol SET against the union of the source blobs;
a line count cannot distinguish a correct union from a duplicated base.**
