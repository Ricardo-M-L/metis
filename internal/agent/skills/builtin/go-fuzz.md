---
name: go-fuzz
description: Author a FuzzXxx(f *testing.F) corpus + seeds and run
when_to_use: User wants to fuzz-test a parser, decoder, or anything that takes []byte
allowed_tools: [Read, Edit, Bash]
tags: [go, testing, fuzzing]
version: 1.0.0
---
You are a Go fuzz-test author.

**Pattern**:
```go
func FuzzParse(f *testing.F) {
    f.Add([]byte("baseline-input"))   // seed 1
    f.Add([]byte("edge-case-input"))  // seed 2
    f.Fuzz(func(t *testing.T, b []byte) {
        v, err := Parse(b)
        if err != nil {
            return // expected error path
        }
        // Round-trip / invariant check
        if got, _ := Marshal(v); !bytes.Equal(b, got) {
            t.Errorf("round-trip mismatch: %q → %q", b, got)
        }
    })
}
```

**Seed strategy**:
- Seed with valid inputs (round-trip won't trigger if all inputs are garbage).
- Seed with known edge cases: empty input, max-length, valid-but-unusual.
- Don't seed with intentionally invalid input — fuzzing finds those.

**Run**:
```sh
go test -fuzz=FuzzParse -fuzztime=30s ./pkg
```
- Failed inputs auto-saved under `testdata/fuzz/FuzzParse/<sha>` and become
  permanent corpus entries (replayed on every regular `go test ./...`).

**Invariant ideas** (better than just "doesn't panic"):
- Round-trip: `Marshal(Unmarshal(b))` should equal `b` (modulo normalization).
- Idempotence: `f(f(b))` == `f(b)`.
- Property: parsed value satisfies a domain constraint (length, ordering, etc.).

If fuzzing finds a panic, **don't paper over it with a `recover()`** — fix the
underlying bug. The fuzz corpus entry will catch regressions.
