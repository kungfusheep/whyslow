package main

import (
	"fmt"

	"debug/macho"
	glyph "github.com/kungfusheep/glyph"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestVarintMatchesProtobufSpec(t *testing.T) {
	for _, c := range []struct {
		v    uint64
		want []byte
	}{
		{1, []byte{0x01}},
		{150, []byte{0x96, 0x01}}, // the example in the protobuf encoding guide
		{300, []byte{0xac, 0x02}},
	} {
		var p pbuf
		p.varint(c.v)
		if string(p.b) != string(c.want) {
			t.Errorf("varint(%d) = % x, want % x", c.v, p.b, c.want)
		}
	}
}

func testTable(t *testing.T, xml string) *table {
	t.Helper()
	root, err := parseNodes(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	return newTable(root)
}

func TestUserStackLinkRegister(t *testing.T) {
	tb := testTable(t, `<r>
		<kperf-bt id="1"><text-addresses id="2">200 300 0</text-addresses><text-address id="3">100</text-address><register-content id="4">150</register-content></kperf-bt>
		<kperf-bt id="5"><text-addresses ref="2"/><text-address id="6">110</text-address><register-content id="7">200</register-content></kperf-bt>
	</r>`)
	bts := tb.find("kperf-bt")

	got := userStack(tb, bts[0])
	want := []stackPC{{pc: 100}, {pc: 150, lr: true}, {pc: 200}, {pc: 300}}
	if !equalStacks(got, want) {
		t.Errorf("leaf with a fresh link register: got %v, want %v", got, want)
	}

	// a link register equal to the first frame-pointer caller is the same frame, not an extra one;
	// the callers come through an id/ref deduplicated element
	got = userStack(tb, bts[1])
	want = []stackPC{{pc: 110}, {pc: 200}, {pc: 300}}
	if !equalStacks(got, want) {
		t.Errorf("link register repeating the caller: got %v, want %v", got, want)
	}
}

func equalStacks(a, b []stackPC) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSymbolizeBuiltBinary builds whyslow itself (go test binaries carry no DWARF or symbols since
// go 1.22) and resolves one of its functions through the DWARF index.
func TestSymbolizeBuiltBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	exe := filepath.Join(t.TempDir(), "whyslow")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	f, err := macho.Open(exe)
	if err != nil {
		t.Skip("not a Mach-O binary:", err)
	}
	defer f.Close()
	// go names symbols after the package path ("whyslow.benchFocus" for package main of module whyslow)
	var addr uint64
	var name string
	for _, s := range f.Symtab.Syms {
		if strings.HasSuffix(s.Name, ".benchFocus") {
			addr, name = s.Value, strings.TrimPrefix(s.Name, "_")
		}
	}
	if addr == 0 {
		t.Fatal("benchFocus not in the symbol table")
	}
	ix := openDWARF(exe, f)
	if ix == nil {
		t.Fatal("no DWARF index")
	}
	fs := ix.frames(addr + 8)
	if len(fs) == 0 || fs[0].fn != name || filepath.Base(fs[0].file) != "main.go" || fs[0].line == 0 {
		t.Fatalf("frames = %+v, want %s in main.go", fs, name)
	}
}

func TestPprofRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	leaf := &loc{pc: 0x1000, leaf: true, frames: []frame{{fn: "pkg.inner", file: "/src/a.go", line: 7}, {fn: "pkg.outer", file: "/src/a.go", line: 20}}}
	caller := &loc{pc: 0x2000, frames: []frame{{fn: "pkg.main", file: "/src/m.go", line: 3}}}
	var d counts
	d[evCycles] = 4000
	p := &profile{samples: []stackSample{{locs: []*loc{leaf, caller}, delta: d}}}
	ev := &events{name: "L1D misses", sampleType: "l1d_misses", samples: 1, locs: []*loc{{pc: 0x1004, leaf: true, frames: []frame{{fn: "pkg.inner", file: "/src/a.go", line: 8}}}}}

	path := filepath.Join(t.TempDir(), "p.pb.gz")
	if err := writePprof(path, p, []*events{ev}, 1e9); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "tool", "pprof", "-raw", path).CombinedOutput()
	if err != nil {
		t.Fatalf("pprof rejected the profile: %v\n%s", err, out)
	}
	raw := string(out)
	for _, want := range []string{"cycles/count", "l1d_misses/count", "pkg.inner", "pkg.outer", "pkg.main"} {
		if !strings.Contains(raw, want) {
			t.Errorf("pprof -raw output missing %q:\n%s", want, raw)
		}
	}
	// the cycle sample has the full stack; the event sample borrows pkg.inner's caller (pkg.main)
	for _, re := range []string{`\s4000\s+0: 1 2\s`, `\s0\s+1: 3 2\s`} {
		if !regexp.MustCompile(re).MatchString(raw) {
			t.Errorf("pprof -raw output has no sample matching %s:\n%s", re, raw)
		}
	}
}

func TestParseBenchmarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	os.WriteFile(path, []byte("goos: darwin\nBenchmarkSmallPayload-18   \t42719638\t        54.72 ns/op\t       0 B/op\t       0 allocs/op\nPASS\n"), 0o644)
	bs := parseBenchmarks(path)
	if len(bs) != 1 || bs[0].name != "BenchmarkSmallPayload-18" || bs[0].nsPerOp != 54.72 || bs[0].iterCount != 42719638 {
		t.Fatalf("got %+v", bs)
	}
}

func TestBenchFocus(t *testing.T) {
	for in, want := range map[string]string{
		"^BenchmarkSmallPayload$": ".BenchmarkSmallPayload",
		"SmallPayload":            ".BenchmarkSmallPayload",
		"Small|Large":             "", // a real pattern: report everything
	} {
		if got := benchFocus(in); got != want {
			t.Errorf("benchFocus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatting(t *testing.T) {
	if got := sig(0.0000239); got != "2.39e-05" {
		t.Errorf("sig = %q", got)
	}
	if got := where("/x/ptrconvert.zig", 0); got != "ptrconvert.zig:(generated)" {
		t.Errorf("where = %q", got)
	}
	if got := commas(1234567); got != "1,234,567" {
		t.Errorf("commas = %q", got)
	}
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		full := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a_test.go", "package a\nimport \"testing\"\nfunc BenchmarkTop(b *testing.B) {}\nfunc Benchmarkfoo(b *testing.B) {}\nfunc BenchmarkArgs(b *testing.B, n int) {}\nfunc TestX(t *testing.T) {}\n", 0o644)
	write("sub/b_test.go", "package sub\nimport \"testing\"\nfunc BenchmarkNested(b *testing.B) {}\n", 0o644)
	write("vendor/v/v_test.go", "package v\nimport \"testing\"\nfunc BenchmarkVendored(b *testing.B) {}\n", 0o644)
	write("build.zig", "", 0o644)
	write("zig-out/bin/bench", "", 0o755)
	write("zig-out/bin/notes.txt", "", 0o644)
	write(".whyslow", "# comment\nSmallPayload = zig-out/bin/bench -filter SmallPayload\nbroken =\n", 0o644)

	var got []string
	for _, c := range detect(root) {
		got = append(got, c.Kind+" "+c.Name+" "+c.Where+" "+strings.Join(c.argv, " "))
	}
	want := []string{
		"file SmallPayload zig-out/bin/bench -filter SmallPayload zig-out/bin/bench -filter SmallPayload",
		"go BenchmarkTop . ",
		"go BenchmarkNested ./sub ",
		"zig bench zig-out/bin/bench " + filepath.Join(root, "zig-out", "bin", "bench"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("detect:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := detect(t.TempDir()); len(c) != 0 {
		t.Errorf("empty dir detected %v", c)
	}
}

func TestANSISpans(t *testing.T) {
	spans := ansiSpans("  \x1b[1mhot\x1b[0m line \x1b[2mcode\x1b[0m")
	var got []string
	for _, s := range spans {
		got = append(got, fmt.Sprintf("%q:%d", s.Text, s.Style.Attr))
	}
	want := []string{`"  ":0`, fmt.Sprintf(`"hot":%d`, glyph.AttrBold), `" line ":0`, fmt.Sprintf(`"code":%d`, glyph.AttrDim)}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRunWritesProgressToLog(t *testing.T) {
	var log strings.Builder
	_, err := run(t.Context(), options{out: t.TempDir()}, []string{"definitely-not-a-command-whyslow"}, &log)
	if err == nil {
		t.Fatal("expected an error for a missing command")
	}
	if _, err := run(t.Context(), options{out: t.TempDir(), bench: "X"}, []string{t.TempDir()}, &log); err == nil {
		t.Fatal("expected go test -c to fail in an empty dir")
	}
	if !strings.Contains(log.String(), "building ") {
		t.Errorf("build progress didn't reach the log writer: %q", log.String())
	}
}

func TestCoreTiers(t *testing.T) {
	tp := testTable(t, `<r><schema><col><mnemonic>thread</mnemonic></col><col><mnemonic>core</mnemonic></col></schema>
		<row><thread id="1" fmt="Main Thread (0x1) (bench, pid: 42)"/><core id="2" fmt="CPU 14 (S Core)">14</core></row>
		<row><thread ref="1"/><core ref="2"/></row>
		<row><thread ref="1"/><core id="3" fmt="CPU 3 (P Core)">3</core></row>
		<row><thread id="4" fmt="other (0x2) (xctrace, pid: 7)"/><core ref="3"/></row>
	</r>`)
	got := coreTiers(tp, 42)
	if len(got) != 2 || got['S'] != 2 || got['P'] != 1 {
		t.Fatalf("tiers = %v, want S:2 P:1 (pid 7 excluded)", got)
	}

	levels := []perfLevel{{"Super", 6}, {"Performance", 12}}
	clean := map[byte]int{'S': 999, 'P': 1} // 0.1% off the fast cores is noise, not skew
	var b strings.Builder
	writeCores(&b, []recordingTiers{{"counters", "the counters", clean}, {"top-down", "where the cycles go", map[byte]int{'S': 1000}}, {"empty", "", nil}}, levels)
	if b.String() != "  ran on: 100.0% Super, 0.1% Performance cores in all 2 recordings\n" {
		t.Errorf("clean runs:\n%q", b.String())
	}

	// a later recording landing on slower cores is the case a counters-only check would miss
	b.Reset()
	writeCores(&b, []recordingTiers{{"counters", "the counters", clean}, {"L1D misses", "the L1D misses shares", got}}, levels)
	out := b.String()
	for _, want := range []string{"counters             99.9% Super, 0.1% Performance cores", "L1D misses           66.7% Super, 33.3% Performance cores",
		"⚠ the L1D misses recording spent 33.3% of its samples on slower cores than the 6 Super cores", "so the L1D misses shares can't be trusted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "⚠") != 1 {
		t.Errorf("only the skewed recording should warn:\n%s", out)
	}
}

func TestPerfLevels(t *testing.T) {
	levels := perfLevels()
	if _, err := exec.LookPath("sysctl"); err != nil || len(levels) == 0 {
		t.Skip("no hw.perflevel sysctls here")
	}
	for _, l := range levels {
		if l.name == "" || l.cores <= 0 {
			t.Errorf("bad level %+v in %+v", l, levels)
		}
	}
}
