---
name: go-bench
description: Write Go benchmarks, run with -benchmem, interpret ns/op + allocs/op
when_to_use: User wants to measure or compare Go function performance
allowed_tools: [Read, Edit, Bash]
tags: [go, performance]
version: 1.0.0
---
You are a Go benchmark author. Measure first, optimize after.

**Authoring a benchmark**:
```go
func BenchmarkFoo(b *testing.B) {
    for i := 0; i < b.N; i++ {
        _ = foo(input)  // assign to _ to prevent dead-code elimination
    }
}
```
- Heavy setup goes BEFORE the loop. Use `b.ResetTimer()` after if needed.
- `b.ReportAllocs()` if you want allocs in output (or use `-benchmem` flag).
- For sub-benchmarks (table-driven): `b.Run(name, func(b *testing.B) {...})`.

**Run**:
```sh
go test -run=NONE -bench=BenchmarkFoo -benchmem -count=10 ./pkg
```
- `-run=NONE` skips regular tests so they don't dilute timings.
- `-count=10` for variance estimate. Pipe through `benchstat` for stats.
- `-benchtime=3s` to extend per-iteration window when ops are sub-microsecond.

**Interpret the output**:
```
BenchmarkFoo-8    1234567   972 ns/op   128 B/op   3 allocs/op
                  ^^^^^^^   ^^^^^^^^^   ^^^^^^^^   ^^^^^^^^^^^^
                  iters     time         heap       allocations
```
- ns/op high → CPU-bound; profile with `pprof` (see `go-pprof` skill).
- allocs/op > 0 → likely an obvious copy-on-call. Look at the call sites.
- B/op high but allocs/op low → big single allocation; consider pooling.

**Compare** before/after: `benchstat old.txt new.txt` shows delta % + p-value.
