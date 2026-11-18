package main

// analyze.go turns counter samples into attributed costs. each sample carries a thread's cumulative
// PMU counters; the delta since that thread's previous sample is charged to the sample's stack, the
// same statistical attribution a timer-based profiler uses for time.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type counts [numEvents]float64

// maxInterval bounds an attributable delta at 1.5x the instrument's 1ms sampling period.
const maxInterval = 1_500_000

func (c *counts) add(o counts) {
	for i := range c {
		c[i] += o[i]
	}
}

// loc is one address in a stack and the source frames it resolves to (innermost first).
type loc struct {
	pc     uint64
	leaf   bool
	lr     bool // came from the link register, which may be stale
	frames []frame
}

// stackPC is a raw stack entry before symbolization.
type stackPC struct {
	pc uint64
	lr bool
}

type stackSample struct {
	locs  []*loc // leaf first
	delta counts
	nanos uint64 // wall time covered by the delta
}

type profile struct {
	samples []stackSample
	locs    map[pcKey]*loc
	sym     *symbolizer
	mainExe string
	tiers   map[byte]int // the process's time profiler samples per core tier letter (S, P, E)
}

// coreTiers counts pid's samples by the tier Instruments labels each core with, e.g. "CPU 14 (S Core)".
func coreTiers(tp *table, pid int) map[byte]int {
	pidTag := fmt.Sprintf("pid: %d)", pid)
	tiers := map[byte]int{}
	for _, row := range tp.rows {
		th, core := tp.cell(row, "thread"), tp.cell(row, "core")
		if th == nil || core == nil || !strings.Contains(th.attr("fmt"), pidTag) {
			continue
		}
		if _, label, ok := strings.Cut(core.attr("fmt"), "("); ok && label != "" {
			tiers[label[0]]++
		}
	}
	return tiers
}

// imageLoads collects each image's load address from the Time Profiler samples in a trace, plus the
// names Instruments symbolicated for each sampled address (used for images without DWARF).
func imageLoads(tp *table) (map[string]uint64, map[string][]named, error) {
	loads := map[string]uint64{}
	for _, b := range tp.find("binary") {
		if p, l := b.attr("path"), b.attr("load-addr"); p != "" && l != "" {
			if v, err := strconv.ParseUint(strings.TrimPrefix(l, "0x"), 16, 64); err == nil {
				loads[p] = v
			}
		}
	}
	if len(loads) == 0 {
		return nil, nil, fmt.Errorf("no image load addresses in the trace (did Time Profiler record?)")
	}
	known := map[string][]named{}
	seen := map[uint64]bool{}
	for _, f := range tp.find("frame") {
		f = tp.resolve(f)
		name := f.attr("name")
		addr, err := strconv.ParseUint(strings.TrimPrefix(f.attr("addr"), "0x"), 16, 64)
		bin := tp.resolve(f.child("binary"))
		if err != nil || bin == nil || name == "" || strings.HasPrefix(name, "0x") || seen[addr] {
			continue
		}
		seen[addr] = true
		known[bin.attr("path")] = append(known[bin.attr("path")], named{addr, name})
	}
	return loads, known, nil
}

// loadProfile reads the counter samples for pid and attributes counter deltas to their stacks.
func loadProfile(trace string, pid int, mainExe string) (*profile, error) {
	tp, err := exportTable(trace, "time-profile")
	if err != nil {
		return nil, err
	}
	loads, known, err := imageLoads(tp)
	if err != nil {
		return nil, err
	}

	kt, err := exportTable(trace, "kdebug-counters-with-time-sample")
	if err != nil {
		return nil, err
	}
	p := &profile{locs: map[pcKey]*loc{}, sym: newSymbolizer(loads, known), mainExe: mainExe, tiers: coreTiers(tp, pid)}

	// the counters are the core's, not the thread's: a delta is only meaningful between consecutive
	// samples on the same core, and only attributable when the same thread ran for the whole interval
	type last struct {
		c      [numEvents]uint64
		t      uint64
		thread string
	}
	prev := map[string]last{}
	pidTag := fmt.Sprintf("pid: %d)", pid)
	type raw struct {
		pcs   []stackPC
		delta counts
		nanos uint64
	}
	var raws []raw
	pcs := map[uint64]bool{}
	for _, row := range kt.rows {
		cur, ok := parseCounters(kt.cell(row, "counters-array"))
		if !ok {
			continue
		}
		core := valueOf(kt.cell(row, "core-index"))
		th := kt.cell(row, "thread")
		thread := ""
		if th != nil {
			thread = th.attr("fmt")
		}
		t, _ := strconv.ParseUint(valueOf(kt.cell(row, "time")), 10, 64)
		before, seen := prev[core]
		prev[core] = last{cur, t, thread}
		// a long gap means the thread was off this core for a while: the delta isn't one interval of its work
		if !seen || before.thread != thread || !strings.Contains(thread, pidTag) || t-before.t > maxInterval {
			continue
		}
		var d counts
		valid := true
		for i := range cur {
			if cur[i] < before.c[i] {
				valid = false
				break
			}
			d[i] = float64(cur[i] - before.c[i])
		}
		if !valid {
			continue
		}
		stack := userStack(kt, kt.cell(row, "cp-user-callstack"))
		if len(stack) == 0 {
			continue
		}
		for _, e := range stack {
			pcs[e.pc] = true
		}
		raws = append(raws, raw{stack, d, t - before.t})
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("no counter samples for pid %d", pid)
	}

	p.sym.resolveFallbacks(pcs)
	for _, r := range raws {
		s := stackSample{delta: r.delta, nanos: r.nanos}
		for i, e := range r.pcs {
			s.locs = append(s.locs, p.loc(e, i == 0))
		}
		p.samples = append(p.samples, s)
	}
	return p, nil
}

func (p *profile) loc(e stackPC, leaf bool) *loc {
	k := pcKey{e.pc, leaf}
	if l := p.locs[k]; l != nil && l.lr == e.lr {
		return l
	}
	l := &loc{pc: e.pc, leaf: leaf, lr: e.lr, frames: p.sym.frames(e.pc, leaf)}
	p.locs[k] = l
	return l
}

func valueOf(n *node) string {
	if n == nil {
		return ""
	}
	return n.value()
}

func parseCounters(n *node) ([numEvents]uint64, bool) {
	var out [numEvents]uint64
	if n == nil {
		return out, false
	}
	fields := strings.Fields(strings.TrimSpace(n.text.String()))
	if len(fields) < numEvents {
		return out, false
	}
	for i := range out {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = v
	}
	return out, true
}

// userStack decodes a kperf backtrace: the pc, then the frame-pointer walk (callers, leaf first). the
// link register only holds a real frame while the pc is in a leaf that hasn't pushed its frame yet, so
// it is used unless it repeats the first caller or points back into the pc's own function.
func userStack(t *table, bt *node) []stackPC {
	if bt == nil || bt.tag != "kperf-bt" {
		return nil
	}
	pcNode := t.resolve(bt.child("text-address"))
	pc, err := strconv.ParseUint(valueOf(pcNode), 10, 64)
	if err != nil || pc == 0 {
		return nil
	}
	var callers []uint64
	if cs := t.resolve(bt.child("text-addresses")); cs != nil {
		for _, f := range strings.Fields(cs.text.String()) {
			if v, err := strconv.ParseUint(f, 10, 64); err == nil && v != 0 {
				callers = append(callers, v)
			}
		}
	}
	stack := []stackPC{{pc: pc}}
	if lrNode := t.resolve(bt.child("register-content")); lrNode != nil {
		if lr, err := strconv.ParseUint(valueOf(lrNode), 10, 64); err == nil && lr != 0 && (len(callers) == 0 || lr != callers[0]) {
			stack = append(stack, stackPC{pc: lr, lr: true})
		}
	}
	for _, c := range callers {
		stack = append(stack, stackPC{pc: c})
	}
	return stack
}

// stats are aggregated costs for one function or source line.
type stats struct {
	name  string
	file  string
	line  int
	self  counts
	total counts
}

type summary struct {
	total    counts
	nanos    uint64
	samples  int
	funcs    []*stats // by self cycles, descending
	lines    map[string][]*stats
	filtered string // the stack filter applied, if any
}

// summarize aggregates samples, optionally only those whose stack passes through a function whose
// name contains focus (e.g. the benchmark being profiled).
func (p *profile) summarize(focus string) *summary {
	s := &summary{lines: map[string][]*stats{}, filtered: focus}
	funcs := map[string]*stats{}
	lines := map[string]*stats{}
	for _, smp := range p.samples {
		stack := p.frames(smp)
		if len(stack) == 0 || (focus != "" && !hasFrame(stack, focus)) {
			continue
		}
		s.samples++
		s.total.add(smp.delta)
		s.nanos += smp.nanos

		leaf := stack[0]
		fs := get(funcs, leaf.fn, func() *stats { return &stats{name: leaf.fn, file: leaf.file} })
		fs.self.add(smp.delta)
		lk := fmt.Sprintf("%s:%d", leaf.file, leaf.line)
		ls := get(lines, leaf.fn+"\x00"+lk, func() *stats { return &stats{name: leaf.fn, file: leaf.file, line: leaf.line} })
		ls.self.add(smp.delta)

		seen := map[string]bool{}
		for _, f := range stack {
			if !seen[f.fn] {
				seen[f.fn] = true
				get(funcs, f.fn, func() *stats { return &stats{name: f.fn, file: f.file} }).total.add(smp.delta)
			}
		}
	}
	for _, f := range funcs {
		s.funcs = append(s.funcs, f)
	}
	sortStats(s.funcs, func(x *stats) float64 { return x.self[evCycles] })
	for _, l := range lines {
		s.lines[l.name] = append(s.lines[l.name], l)
	}
	for _, ls := range s.lines {
		sortStats(ls, func(x *stats) float64 { return x.self[evCycles] })
	}
	return s
}

// frames flattens a sample to source frames, innermost first, dropping a link-register entry that
// turned out to be stale (it resolved into the pc's own function).
func (p *profile) frames(smp stackSample) []frame {
	var out []frame
	for i, l := range smp.locs {
		if l.lr && i > 0 && sameFunction(l, smp.locs[0]) {
			continue
		}
		out = append(out, l.frames...)
	}
	return out
}

// sameFunction compares the outermost (non-inlined) function of two locations.
func sameFunction(a, b *loc) bool {
	if len(a.frames) == 0 || len(b.frames) == 0 {
		return false
	}
	return a.frames[len(a.frames)-1].fn == b.frames[len(b.frames)-1].fn
}

func sortStats(s []*stats, key func(*stats) float64) {
	sort.Slice(s, func(i, j int) bool { return key(s[i]) > key(s[j]) })
}

func hasFrame(stack []frame, focus string) bool {
	for _, f := range stack {
		if strings.Contains(f.fn, focus) {
			return true
		}
	}
	return false
}

func get[K comparable, V any](m map[K]V, k K, mk func() V) V {
	v, ok := m[k]
	if !ok {
		v = mk()
		m[k] = v
	}
	return v
}

// events holds the samples of one event-sampling run: each is one PMU interrupt after a fixed
// number of events, at the exact pc responsible.
type events struct {
	name       string // for the report, e.g. "L1D misses"
	sampleType string // for pprof, e.g. "l1d_misses"
	samples    int
	locs       []*loc // leaf location of each sample
	byFunc     map[string]int
	byLine     map[string]int // fn + "\x00" + file:line
	tiers      map[byte]int   // this recording's samples per core tier
}

func loadEvents(trace string, pid int, name, sampleType string) (*events, error) {
	tp, err := exportTable(trace, "time-profile")
	if err != nil {
		return nil, err
	}
	loads, known, err := imageLoads(tp)
	if err != nil {
		return nil, err
	}
	t, err := exportTable(trace, "pmi-sample")
	if err != nil {
		return nil, err
	}
	sym := newSymbolizer(loads, known)
	pidTag := fmt.Sprintf("pid: %d)", pid)
	var pcs []uint64
	for _, row := range t.rows {
		th := t.cell(row, "thread")
		if th == nil || !strings.Contains(th.attr("fmt"), pidTag) {
			continue
		}
		if pc, err := strconv.ParseUint(valueOf(t.cell(row, "program-counter")), 10, 64); err == nil && pc != 0 {
			pcs = append(pcs, pc)
		}
	}
	set := map[uint64]bool{}
	for _, pc := range pcs {
		set[pc] = true
	}
	sym.resolveFallbacks(set)
	ev := &events{name: name, sampleType: sampleType, byFunc: map[string]int{}, byLine: map[string]int{}, tiers: coreTiers(tp, pid)}
	for _, pc := range pcs {
		l := &loc{pc: pc, leaf: true, frames: sym.frames(pc, true)}
		f := l.frames[0]
		ev.samples++
		ev.locs = append(ev.locs, l)
		ev.byFunc[f.fn]++
		ev.byLine[f.fn+"\x00"+fmt.Sprintf("%s:%d", f.file, f.line)]++
	}
	return ev, nil
}
