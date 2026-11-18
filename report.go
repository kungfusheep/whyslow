package main

// report.go renders the terminal report: what kind of stall dominates, which functions and lines it
// comes from, and the source for those lines.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type benchResult struct {
	line      string
	name      string
	nsPerOp   float64
	allocs    string
	bytesOp   string
	iterCount int64
}

var benchLine = regexp.MustCompile(`^(Benchmark\S+)\s+(\d+)\s+([\d.]+) ns/op(?:\s+(\d+) B/op)?(?:\s+(\d+) allocs/op)?`)

func parseBenchmarks(path string) []benchResult {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []benchResult
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := benchLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		ns, _ := strconv.ParseFloat(m[3], 64)
		n, _ := strconv.ParseInt(m[2], 10, 64)
		out = append(out, benchResult{line: strings.TrimSpace(sc.Text()), name: m[1], nsPerOp: ns, bytesOp: m[4], allocs: m[5], iterCount: n})
	}
	return out
}

type report struct {
	title   string
	sum     *summary
	td      *topDown
	l1d     *events // L1D load-miss locations (nil when not recorded)
	misp    *events // branch-mispredict locations
	benches []benchResult
	pprof   string
	trace   string
	top     int
	runs    []recordingTiers
	levels  []perfLevel
}

// recordingTiers is one recording's samples per core tier, and the report section it feeds.
type recordingTiers struct {
	name, section string
	tiers         map[byte]int
}

// share is a function's or line's fraction of an event run's samples, "-" without that run.
func share(ev *events, n int) string {
	if ev == nil || ev.samples == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(ev.samples))
}

func evCount(ev *events, m func(*events) map[string]int, key string) int {
	if ev == nil {
		return 0
	}
	return m(ev)[key]
}

func byFunc(ev *events) map[string]int { return ev.byFunc }
func byLine(ev *events) map[string]int { return ev.byLine }

func (r *report) write(w io.Writer) {
	s := r.sum
	fmt.Fprintf(w, "\n%s\n", bold(r.title))
	scope := "whole process"
	if s.filtered != "" {
		scope = "stacks through " + s.filtered
	}
	fmt.Fprintf(w, "%s samples (1ms), %s\n\n", commas(int64(s.samples)), scope)

	for _, b := range r.benches {
		fmt.Fprintf(w, "  %s\n", b.line)
	}
	if len(r.benches) > 0 {
		fmt.Fprintln(w)
	}

	if r.td != nil {
		fmt.Fprintf(w, "%s\n", bold("where the cycles go"))
		td := *r.td
		bar(w, "useful work", td.useful)
		bar(w, "front end (delivery)", td.delivery)
		bar(w, "back end (processing)", td.processing)
		bar(w, "mispredicted (discarded)", td.discarded)
		fmt.Fprintf(w, "  → %s\n\n", topDownVerdict(td))
	}

	t := s.total
	fmt.Fprintf(w, "%s\n", bold("counters"))
	ghz := 0.0
	if s.nanos > 0 {
		ghz = t[evCycles] / float64(s.nanos)
	}
	fmt.Fprintf(w, "  IPC %.2f   clock %.2f GHz   L1D load misses %s   L1D store misses %s   L1I misses %s   mispredicts %s   dTLB misses %s   (per 1k instructions)\n",
		ratio(t[evInstructions], t[evCycles]), ghz,
		pki(t, evL1DLoadMiss), pki(t, evL1DStoreMiss), pki(t, evL1IMiss), pki(t, evMispredict), pki(t, evDTLBMiss))
	fmt.Fprintf(w, "  %.1f%% of cycles waiting on an L1D miss\n", 100*ratio(t[evL1DWait], t[evCycles]))
	if len(r.benches) == 1 && ghz > 0 {
		perOp := r.benches[0].nsPerOp * ghz / t[evCycles] // events per op = total × (cycles per op / total cycles)
		fmt.Fprintf(w, "  per op: %s cycles, %s instructions, %s L1D load misses, %s mispredicts\n",
			commasF(t[evCycles]*perOp), commasF(t[evInstructions]*perOp), sig(t[evL1DLoadMiss]*perOp), sig(t[evMispredict]*perOp))
	}
	writeCores(w, r.runs, r.levels)
	fmt.Fprintln(w)

	fmt.Fprintf(w, "%s\n", bold("hottest functions (self)"))
	fmt.Fprintf(w, "  %7s  %8s  %8s  %s\n", "cycles", "L1D miss", "mispred", "function")
	shown := 0
	for _, f := range s.funcs {
		if shown == r.top || f.self[evCycles] == 0 {
			break
		}
		shown++
		cyc := f.self[evCycles] / t[evCycles]
		l1d, misp := evCount(r.l1d, byFunc, f.name), evCount(r.misp, byFunc, f.name)
		fmt.Fprintf(w, "  %6.1f%%  %8s  %8s  %s%s\n", 100*cyc, share(r.l1d, l1d), share(r.misp, misp), f.name, dim(hotspot(cyc, r.l1d, l1d, r.misp, misp)))
	}
	for _, ev := range []*events{r.l1d, r.misp} {
		if ev != nil {
			if top := topUncovered(ev, s.funcs, r.top); top != "" {
				fmt.Fprintf(w, "  %s\n", dim(top))
			}
		}
	}
	fmt.Fprintln(w)

	for i, f := range s.funcs {
		if i == 3 || f.self[evCycles] < 0.03*t[evCycles] {
			break
		}
		lines := s.lines[f.name]
		if len(lines) == 0 || lines[0].file == "" {
			continue
		}
		fmt.Fprintf(w, "%s %s\n", bold("hottest lines in"), f.name)
		for j, l := range lines {
			if j == 6 || l.self[evCycles] < 0.005*t[evCycles] {
				break
			}
			key := l.name + "\x00" + fmt.Sprintf("%s:%d", l.file, l.line)
			fmt.Fprintf(w, "  %5.1f%%  L1D %6s  misp %6s  %s  %s\n",
				100*l.self[evCycles]/t[evCycles], share(r.l1d, evCount(r.l1d, byLine, key)), share(r.misp, evCount(r.misp, byLine, key)),
				where(l.file, l.line), dim(sourceLine(l.file, l.line)))
		}
		fmt.Fprintln(w)
	}

	for _, ev := range []*events{r.l1d, r.misp} {
		if ev == nil || ev.samples == 0 {
			continue
		}
		fmt.Fprintf(w, "%s (%d samples, whole process)\n", bold("where the "+ev.name+" happen"), ev.samples)
		if ev.samples < 20 {
			fmt.Fprintf(w, "  too few to attribute: this run causes almost no %s (mostly process startup)\n\n", ev.name)
			continue
		}
		type kv struct {
			k string
			n int
		}
		var lines []kv
		for k, n := range ev.byLine {
			lines = append(lines, kv{k, n})
		}
		sort.Slice(lines, func(a, b int) bool { return lines[a].n > lines[b].n })
		for i, l := range lines {
			if i == 6 {
				break
			}
			fn, at, _ := strings.Cut(l.k, "\x00")
			file, ln := splitLine(at)
			fmt.Fprintf(w, "  %5.1f%%  %s  %s  %s\n", 100*float64(l.n)/float64(ev.samples), where(file, ln), fn, dim(sourceLine(file, ln)))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "%s\n", bold("explore"))
	fmt.Fprintf(w, "  go tool pprof -http=: %s\n", r.pprof)
	fmt.Fprintf(w, "    SAMPLE menu: cycles, l1d_misses, branch_mispredicts\n")
	fmt.Fprintf(w, "  open %s   (Instruments)\n\n", r.trace)
}

func topDownVerdict(td topDown) string {
	switch {
	case td.discarded > 0.15:
		return "significant time lost to branch mispredictions"
	case td.processing > td.delivery && td.processing > 0.3:
		return "back-end bound: data-cache misses or long dependency chains (loads waiting on stores, serial arithmetic)"
	case td.delivery > 0.3:
		return "front-end bound: instruction fetch can't keep up (many calls/indirect jumps, large or scattered code)"
	case td.useful > 0.6:
		return "mostly doing useful work; speedups now come from doing less work"
	default:
		return "mixed: no single bottleneck dominates"
	}
}

// hotspot flags a function that causes far more than its share of misses or mispredicts.
func hotspot(cycles float64, l1d *events, l1dN int, misp *events, mispN int) string {
	frac := func(ev *events, n int) float64 {
		if ev == nil || ev.samples < 20 {
			return 0 // too few samples to call it
		}
		return float64(n) / float64(ev.samples)
	}
	switch {
	case frac(l1d, l1dN) > 0.1 && frac(l1d, l1dN) > 2*cycles:
		return "  ← concentrates L1D misses"
	case frac(misp, mispN) > 0.1 && frac(misp, mispN) > 2*cycles:
		return "  ← concentrates mispredicts"
	}
	return ""
}

// topUncovered names an event's biggest source when it isn't among the listed hot functions.
func topUncovered(ev *events, funcs []*stats, n int) string {
	listed := map[string]bool{}
	for i, f := range funcs {
		if i == n {
			break
		}
		listed[f.name] = true
	}
	best, bestN := "", 0
	for fn, c := range ev.byFunc {
		if !listed[fn] && c > bestN {
			best, bestN = fn, c
		}
	}
	if best == "" || float64(bestN) < 0.1*float64(ev.samples) {
		return ""
	}
	return fmt.Sprintf("also: %.0f%% of %s in %s (not in the cycle list)", 100*float64(bestN)/float64(ev.samples), ev.name, best)
}

// where renders a source position; line 0 is compiler-generated code (prologues, spills, glue).
func where(file string, line int) string {
	switch {
	case file == "" || file == ".":
		return "(no source)"
	case line == 0:
		return filepath.Base(file) + ":(generated)"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

func splitLine(at string) (string, int) {
	i := strings.LastIndex(at, ":")
	if i < 0 {
		return at, 0
	}
	n, _ := strconv.Atoi(at[i+1:])
	return at[:i], n
}

func bar(w io.Writer, label string, frac float64) {
	n := int(frac*40 + 0.5)
	fmt.Fprintf(w, "  %-26s %5.1f%%  %s\n", label, 100*frac, strings.Repeat("█", n))
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func pki(c counts, ev int) string {
	if c[evInstructions] == 0 {
		return "-"
	}
	return sig(1000 * c[ev] / c[evInstructions])
}

var sourceCache = map[string][]string{}

func sourceLine(path string, line int) string {
	lines, ok := sourceCache[path]
	if !ok {
		if b, err := os.ReadFile(path); err == nil {
			lines = strings.Split(string(b), "\n")
		}
		sourceCache[path] = lines
	}
	if line <= 0 || line > len(lines) {
		return ""
	}
	s := strings.TrimSpace(lines[line-1])
	if len(s) > 90 {
		s = s[:87] + "..."
	}
	return s
}

var isTTY = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

func bold(s string) string {
	if !isTTY {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

func dim(s string) string {
	if !isTTY || s == "" {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

// perfLevel is one core tier, fastest first, as sysctl hw.perflevelN describes it.
type perfLevel struct {
	name  string // "Super", "Performance", "Efficiency"
	cores int
}

func perfLevels() []perfLevel {
	// sysctl exits 0 even for a level that doesn't exist, so the count comes from hw.nperflevels
	n := sysctlInt("hw.nperflevels")
	var out []perfLevel
	for i := range n {
		name, err := exec.Command("sysctl", "-n", fmt.Sprintf("hw.perflevel%d.name", i)).Output()
		if s := strings.TrimSpace(string(name)); err == nil && s != "" {
			out = append(out, perfLevel{s, sysctlInt(fmt.Sprintf("hw.perflevel%d.logicalcpu", i))})
		}
	}
	return out
}

func sysctlInt(name string) int {
	out, _ := exec.Command("sysctl", "-n", name).Output()
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// writeCores says which core tiers each recording ran on, and warns about any that spent real time
// off the fastest tier: macOS can't pin threads, so a busy machine pushes benchmarks onto slower
// cores, and each recording is a separate launch that can land differently.
func writeCores(w io.Writer, runs []recordingTiers, levels []perfLevel) {
	if len(levels) == 0 {
		return
	}
	fast := levels[0]
	all := map[byte]int{}
	var measured, skewed []recordingTiers
	for _, r := range runs {
		if total(r.tiers) == 0 {
			continue
		}
		measured = append(measured, r)
		for k, n := range r.tiers {
			all[k] += n
		}
		if slowShare(r.tiers, fast) > 0.01 {
			skewed = append(skewed, r)
		}
	}
	if len(measured) == 0 {
		return
	}
	if len(skewed) == 0 {
		scope := "the " + measured[0].name + " recording"
		if len(measured) > 1 {
			scope = fmt.Sprintf("all %d recordings", len(measured))
		}
		fmt.Fprintf(w, "  ran on: %s cores in %s\n", tierShares(all, levels), scope)
		return
	}
	fmt.Fprintf(w, "  ran on:\n")
	for _, r := range measured {
		fmt.Fprintf(w, "    %-20s %s cores\n", r.name, tierShares(r.tiers, levels))
	}
	for _, r := range skewed {
		fmt.Fprintf(w, "  %s\n", bold(fmt.Sprintf("⚠ the %s recording spent %.1f%% of its samples on slower cores than the %d %s cores: something else was probably busy, so %s can't be trusted. re-run on a quiet machine",
			r.name, 100*slowShare(r.tiers, fast), fast.cores, fast.name, r.section)))
	}
}

func total(tiers map[byte]int) int {
	n := 0
	for _, v := range tiers {
		n += v
	}
	return n
}

func slowShare(tiers map[byte]int, fast perfLevel) float64 {
	return 1 - float64(tiers[fast.name[0]])/float64(total(tiers))
}

func tierShares(tiers map[byte]int, levels []perfLevel) string {
	var parts []string
	for _, l := range levels {
		if n := tiers[l.name[0]]; n > 0 {
			parts = append(parts, fmt.Sprintf("%.1f%% %s", 100*float64(n)/float64(total(tiers)), l.name))
		}
	}
	return strings.Join(parts, ", ")
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// sig formats small rates with three significant digits so they don't round to zero.
func sig(v float64) string {
	if v == 0 {
		return "0"
	}
	return strconv.FormatFloat(v, 'g', 3, 64)
}

func commasF(f float64) string { return commas(int64(f + 0.5)) }
