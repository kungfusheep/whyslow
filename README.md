# whyslow

Run a benchmark, get told *why* it's slow. whyslow records Apple Silicon PMU counters (cycles, instructions, cache misses, mispredicts) with stacks through Instruments, maps them to source with inline frames, and prints:
- which kind of stall dominates;
- which functions and lines it comes from;
- the code on those lines.

It also writes a **pprof profile with cycles, L1D-miss and mispredict sample types**, so `go tool pprof -http=:` can draw flame graphs and annotated source for each.

```sh
go install whyslow@latest   # or: go build -o whyslow . in this directory

whyslow -bench '^BenchmarkSmallPayload$' ./jingo             # go benchmarks: builds the test binary for you
whyslow -- ./zig-out/bin/bench -filter SmallPayload           # any native binary: zig, c, c++, rust, swift
```

Run `whyslow` with no arguments to pick from what it finds in the current directory instead:

- **go:** every `func BenchmarkX(b *testing.B)` in the module's test files (read from source, nothing is compiled);
- **zig:** executables in `zig-out/bin` when there's a `build.zig`;
- **named targets** from a `.whyslow` file, for anything detection can't see, like a benchmark binary's `-filter` or a `b.Run` sub-benchmark:

```
# .whyslow: name = command
SmallPayload = zig-out/bin/jinzig-bench-fused -filter SmallPayload
```

Type to filter, `enter` to profile, or `ctrl-r` to profile whatever you typed as a command. Progress streams in while it records, then the report appears in a scrollable view: `p` opens the profile in pprof's web UI, `i` opens the trace in Instruments, `r` re-runs, `x` stops the recording cleanly and `esc` goes back to the list. Flags such as `-benchtime` and `-events=false` apply to every run from the picker.

macOS on Apple Silicon with Xcode installed (it drives `xctrace`). No other dependencies.

## What you get

```
where the cycles go
  useful work                 43.8%  ██████████████████
  front end (delivery)        23.0%  █████████
  back end (processing)       29.2%  ████████████
  mispredicted (discarded)     3.9%  ██

counters
  IPC 5.20   clock 4.51 GHz   L1D load misses 0.0003 ... (per 1k instructions)
  per op: 246 cycles, 1,280 instructions, 0.0004 L1D load misses, 0.0001 mispredicts

hottest functions (self)
   cycles  L1D miss   mispred  function
    35.6%      2.2%     17.2%  jingo.(*StructEncoder).Marshal
    34.8%      4.4%      6.9%  runtime.memmove

hottest lines in jingo.(*StructEncoder).Marshal
   15.9%  L1D   0.0%  misp  17.2%  structencoder.go:58  for i := 0; i < len(e.instructions); i++ {

where the branch mispredicts happen (29 samples, whole process)
   17.2%  structencoder.go:58  jingo.(*StructEncoder).Marshal  for i := 0; ...
```

| Section | Source | Trust it for |
| --- | --- | --- |
| where the cycles go | Instruments' CPU Bottlenecks mode | the overall stall split for the run |
| counters / per op | 1ms timer samples carrying PMU counters | totals for the run (per op when a single Go benchmark ran) |
| ran on | the Time Profiler's core labels (S, P, E) for the process's samples, in each of the four recordings | whether every recording stayed on the fastest cores; a ⚠ names any recording with over 1% of its samples on a slower tier, and the section it skews |
| cycles per function and line | the same timer samples, with full stacks | where the time goes |
| L1D miss / mispred shares | Instruments' `l1d_miss_sampling` / `discarded_sampling` modes: an interrupt every N events at the exact PC | where those events happen |

**Why the columns are shares, not rates.** A timer sample's counter delta covers about 1ms of execution, which for a tight benchmark loop means thousands of iterations through every function. Charging that delta to the one PC the timer landed on gives every function the same average IPC and miss rate. So whyslow never reports per-function rates from timer data. Per-function cache and branch attribution comes only from event-triggered samples, reported as each function's share of the misses. A function with 5% of cycles but 60% of the misses is flagged as concentrating them.

**Event samples have no stacks.** Instruments' event modes record only the PC, so the "where the … happen" sections cover the whole process and can't be filtered to the benchmark. In the pprof profile, each event sample borrows its function's most common call path from the cycle samples. That keeps it in the right place in flame graphs: the leaf function and line are exact, and the callers above them are that function's typical ones.

## Exploring

```sh
go tool pprof -http=: /path/printed/profile.pb.gz
```

The SAMPLE menu switches between `cycles`, `l1d_misses` and `branch_mispredicts`. The source view annotates your files with each, inline frames included. The `.trace` files open in Instruments for the raw data.

## Flags

```
-bench REGEXP   go mode: benchmarks to run (the package dir is the optional argument)
-benchtime 2s   go mode: -test.benchtime
-focus NAME     only count stacks through a function containing NAME (go mode defaults to the benchmark)
-topdown=false  skip the CPU Bottlenecks run
-events=false   skip the L1D-miss and mispredict runs
-limit 60s      hard time limit for each recording
-o DIR          output directory (default: a new one under $TMPDIR/whyslow)
-top 12         functions to list
```

A full run is four recordings of the target: counters, L1D misses, mispredicts and top-down.

## How it works

- **Counters with stacks:** a small embedded Instruments package (`instrument/whyslow.instrpkg`, built once into the user cache with `instrumentbuilder`) defines a `counters-profile` table sampling every 1ms with eight PMU events. Time Profiler runs alongside, because its frames carry each image's ASLR load address.
- **Deltas per core:** the counters in each sample belong to the *core*, not the thread. Deltas are taken between consecutive samples on the same core, and only charged to a stack when the same thread ran for the whole ~1ms interval.
- **Symbolization:** DWARF via `debug/dwarf`, from the binary itself (Go) or its `.dSYM`, created with `dsymutil` when missing (Zig, C, Rust). It resolves inline chains through `DW_TAG_inlined_subroutine` call sites and lines through the line table, and prefers Zig's fully qualified linkage names. System libraries take Instruments' own names, because `atos` misnames functions in the dyld shared cache.
- **pprof:** a minimal dependency-free `profile.proto` encoder, already symbolized, so it works for any language.

## Safety

PMU interrupts are system-wide. An event-triggered sampler on a frequent event with a low threshold can interrupt the machine so often that it wedges, and developing this tool did exactly that once. So:
- whyslow's own instrument only ever samples on a fixed **timer**;
- event sampling uses Instruments' built-in modes, whose thresholds Apple picks;
- every recording has a **hard time limit**;
- whyslow **refuses to start** when the load average already exceeds the core count.
