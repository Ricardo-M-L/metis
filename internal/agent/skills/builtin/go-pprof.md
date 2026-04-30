---
name: go-pprof
description: Capture Go CPU/heap profile, walk hot path, suggest fix
when_to_use: User wants to find why a Go program is slow or uses too much memory
allowed_tools: [Bash, Read, Edit]
tags: [go, performance, profiling]
version: 1.0.0
---
You are a `pprof` co-pilot. Find the hot path; don't guess.

**Capture**:

For a benchmark:
```sh
go test -bench=BenchmarkX -cpuprofile cpu.out -memprofile mem.out ./pkg
```

For a running server (with `net/http/pprof` imported):
```sh
go tool pprof -seconds 30 http://localhost:6060/debug/pprof/profile  # CPU
go tool pprof http://localhost:6060/debug/pprof/heap                  # heap
```

**Analyze**:
```sh
go tool pprof cpu.out
(pprof) top 20          # 20 hottest functions
(pprof) list FuncName   # annotated source with timing
(pprof) web             # SVG flame-graph (needs graphviz)
```

**Reading hot paths**:
- `flat` is time spent IN the function itself (not callees). High flat = work
  happens here. Low flat + high cum = dispatcher; look at callees.
- For heap profiles: alloc_space vs inuse_space. Use inuse to find leaks; alloc
  to find churn (gc pressure).
- Don't trust 1 sample. `-count=10` for benchmarks; longer `-seconds` for live.

**Common findings + fixes**:
- `runtime.mallocgc` high → reduce allocations (pool, batch, pre-size slices)
- `syscall.Read/Write` high → buffer the IO (`bufio.Reader/Writer`)
- regex compile in a hot path → compile once at package init
- `reflect.*` high → replace with code generation or type-specific path
- Mutex contention → `pprof http://.../debug/pprof/mutex` (need `runtime.SetMutexProfileFraction`)
