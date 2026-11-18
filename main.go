// whyslow explains why code is slow: it records PMU counters (cycles, instructions, cache misses,
// mispredicts) with stacks via Instruments, maps them to source, and reports which kind of stall
// dominates and where. it also writes a pprof profile with one sample type per counter.
//
//	whyslow                                       pick a target in the current directory (tui)
//	whyslow [flags] -bench REGEXP [package dir]   profile go benchmarks
//	whyslow [flags] -- command [args...]          profile any native program
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	var o options
	flag.StringVar(&o.bench, "bench", "", "go mode: benchmark regexp to run (as go test -bench)")
	flag.StringVar(&o.benchtime, "benchtime", "2s", "go mode: -test.benchtime")
	flag.DurationVar(&o.limit, "limit", 60*time.Second, "hard time limit for each recording")
	flag.BoolVar(&o.topdown, "topdown", true, "also record Instruments' CPU Bottlenecks breakdown (one more run)")
	flag.BoolVar(&o.events, "events", true, "also record where L1D misses and branch mispredicts happen (two more runs)")
	flag.StringVar(&o.out, "o", "", "output directory (default: a new directory under $TMPDIR/whyslow)")
	flag.StringVar(&o.focus, "focus", "", "only count stacks through a function containing this (go mode defaults to the benchmark)")
	flag.IntVar(&o.top, "top", 12, "functions to list")
	flag.Usage = usage
	flag.Parse()

	if o.bench == "" && flag.NArg() == 0 {
		if err := runTUI(o, "."); err != nil {
			fmt.Fprintln(os.Stderr, "whyslow:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r, err := run(ctx, o, flag.Args(), os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "whyslow:", err)
		os.Exit(1)
	}
	r.write(os.Stdout)
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  whyslow                                       pick something to profile in the current directory
  whyslow [flags] -bench REGEXP [package dir]   profile go benchmarks
  whyslow [flags] -- command [args...]          profile any native program

flags:
`)
	flag.PrintDefaults()
}

type options struct {
	bench, benchtime string
	limit            time.Duration
	topdown, events  bool
	out, focus       string
	top              int
}

// run records and analyses the target, writing progress to log. cancelling ctx stops the current
// recording cleanly (xctrace gets an interrupt and finalizes the trace).
func run(ctx context.Context, o options, args []string, log io.Writer) (*report, error) {
	out, err := outputDir(o.out, o.bench, args)
	if err != nil {
		return nil, err
	}

	var t target
	var title string
	focus := o.focus
	if o.bench != "" {
		pkg := "."
		if len(args) > 0 {
			pkg = args[0]
		}
		if t, err = buildGoBenchmark(ctx, pkg, o.bench, o.benchtime, out, log); err != nil {
			return nil, err
		}
		title = fmt.Sprintf("whyslow · go test -bench %s %s", o.bench, pkg)
		if focus == "" {
			focus = benchFocus(o.bench)
		}
	} else {
		if len(args) == 0 {
			return nil, fmt.Errorf("nothing to profile")
		}
		bin, err := exec.LookPath(args[0])
		if err != nil {
			return nil, err
		}
		abs, _ := filepath.Abs(bin)
		t = target{argv: append([]string{abs}, args[1:]...)}
		title = "whyslow · " + strings.Join(args, " ")
	}

	fmt.Fprintf(log, "recording counters (limit %s)...\n", o.limit)
	rec, err := recordCounters(ctx, t, out, o.limit)
	if err != nil {
		return nil, err
	}
	toc, err := exportTOC(rec.trace)
	if err != nil {
		return nil, err
	}
	pid, _, err := launchedPID(toc)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(log, "symbolizing...")
	p, err := loadProfile(rec.trace, pid, t.argv[0])
	if err != nil {
		return nil, err
	}

	r := &report{title: title, sum: p.summarize(focus), benches: parseBenchmarks(rec.stdout), trace: rec.trace, top: o.top, levels: perfLevels()}
	r.runs = append(r.runs, recordingTiers{"counters", "the counters, functions and lines", p.tiers})
	if r.sum.samples == 0 && focus != "" {
		fmt.Fprintf(log, "no stacks through %q; reporting the whole process\n", focus)
		r.sum = p.summarize("")
	}
	var evs []*events
	if o.events {
		for _, m := range []struct{ run, mode, name, typ string }{
			{"l1d", "l1d_miss_sampling", "L1D misses", "l1d_misses"},
			{"mispredicts", "discarded_sampling", "branch mispredicts", "branch_mispredicts"},
		} {
			fmt.Fprintf(log, "recording %s...\n", m.name)
			ev, err := eventRun(ctx, t, out, m.run, m.mode, m.name, m.typ, o.limit)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				fmt.Fprintf(log, "%s skipped: %v\n", m.name, err)
				continue
			}
			evs = append(evs, ev)
			r.runs = append(r.runs, recordingTiers{m.name, "the " + m.name + " shares", ev.tiers})
			switch m.run {
			case "l1d":
				r.l1d = ev
			case "mispredicts":
				r.misp = ev
			}
		}
	}

	r.pprof = filepath.Join(out, "profile.pb.gz")
	if err := writePprof(r.pprof, p, evs, r.sum.nanos); err != nil {
		return nil, err
	}

	if o.topdown {
		fmt.Fprintln(log, "recording top-down breakdown...")
		if trec, err := recordTopDown(ctx, t, out, o.limit); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			fmt.Fprintln(log, "top-down skipped:", err)
		} else if ttoc, err := exportTOC(trec.trace); err == nil {
			if tpid, _, err := launchedPID(ttoc); err == nil {
				if v, err := loadTopDown(trec.trace, tpid); err == nil {
					r.td = &v
					r.runs = append(r.runs, recordingTiers{"top-down", "the where-the-cycles-go split", v.tiers})
				} else {
					fmt.Fprintln(log, "top-down skipped:", err)
				}
			}
		}
	}
	return r, nil
}

func eventRun(ctx context.Context, t target, out, run, mode, name, typ string, limit time.Duration) (*events, error) {
	rec, err := recordEvents(ctx, t, out, run, mode, limit)
	if err != nil {
		return nil, err
	}
	toc, err := exportTOC(rec.trace)
	if err != nil {
		return nil, err
	}
	pid, _, err := launchedPID(toc)
	if err != nil {
		return nil, err
	}
	return loadEvents(rec.trace, pid, name, typ)
}

func outputDir(dir, bench string, args []string) (string, error) {
	if dir == "" {
		name := "run"
		if bench != "" {
			name = regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(bench, "")
		} else if len(args) > 0 {
			name = filepath.Base(args[0])
		}
		dir = filepath.Join(os.TempDir(), "whyslow", fmt.Sprintf("%s-%s", name, time.Now().Format("20060102-150405")))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".trace") {
			return "", fmt.Errorf("%s already holds a recording; pick an empty -o", dir)
		}
	}
	return dir, nil
}

// buildGoBenchmark compiles the package's test binary and returns the benchmark invocation. it runs
// in the package directory so tests that read testdata work, and uses GOPATH mode when there is no
// go.mod above the package.
func buildGoBenchmark(ctx context.Context, pkg, bench, benchtime, out string, log io.Writer) (target, error) {
	dir, err := filepath.Abs(pkg)
	if err != nil {
		return target{}, err
	}
	bin := filepath.Join(out, filepath.Base(dir)+".test")
	cmd := exec.CommandContext(ctx, "go", "test", "-c", "-vet=off", "-o", bin, ".")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if !hasGoMod(dir) {
		cmd.Env = append(cmd.Env, "GO111MODULE=off")
	}
	cmd.Stdout, cmd.Stderr = log, log
	fmt.Fprintf(log, "building %s...\n", bin)
	if err := cmd.Run(); err != nil {
		return target{}, fmt.Errorf("go test -c: %w", err)
	}
	return target{
		argv: []string{bin, "-test.run", "^$", "-test.bench", bench, "-test.benchtime", benchtime, "-test.benchmem"},
		dir:  dir,
	}, nil
}

func hasGoMod(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// benchFocus turns a simple benchmark regexp like ^BenchmarkSmallPayload$ into a frame filter.
func benchFocus(bench string) string {
	name := strings.Trim(bench, "^$")
	if regexp.QuoteMeta(name) != name {
		return "" // a real pattern: don't guess, report every stack
	}
	if !strings.HasPrefix(name, "Benchmark") {
		name = "Benchmark" + name
	}
	return "." + name
}
