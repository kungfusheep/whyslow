package main

// symbolize.go maps runtime PCs to source. images with DWARF (go binaries embed it; zig, c and rust
// binaries keep it in a .dSYM) are resolved to inline frame chains with file:line. everything else,
// mostly system libraries, falls back to atos for a function name.

import (
	"debug/dwarf"
	"debug/macho"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// frame is one function in a stack, innermost (possibly inlined) first.
type frame struct {
	fn   string
	file string
	line int
}

// named is an address Instruments symbolicated itself.
type named struct {
	addr uint64
	name string
}

type image struct {
	path  string
	known []named // instruments' names for sampled addresses, sorted
	load  uint64  // runtime address of __TEXT
	limit uint64  // first address past the image (estimated for images we can't open)
	slide uint64  // load minus the preferred __TEXT address
	dw    *dwarfIndex
	names map[uint64]string // atos fallback, keyed by runtime pc
}

type symbolizer struct {
	images []*image // sorted by load
	cache  map[pcKey][]frame
}

type pcKey struct {
	pc   uint64
	leaf bool
}

func newSymbolizer(loads map[string]uint64, known map[string][]named) *symbolizer {
	s := &symbolizer{cache: map[pcKey][]frame{}}
	for path, load := range loads {
		im := &image{path: path, load: load, known: known[path]}
		sort.Slice(im.known, func(i, j int) bool { return im.known[i].addr < im.known[j].addr })
		if f, err := macho.Open(path); err == nil {
			if text := f.Segment("__TEXT"); text != nil {
				im.slide = load - text.Addr
				im.limit = load + text.Memsz
			}
			im.dw = openDWARF(path, f)
			f.Close()
		}
		s.images = append(s.images, im)
	}
	sort.Slice(s.images, func(i, j int) bool { return s.images[i].load < s.images[j].load })
	for i, im := range s.images {
		if im.limit == 0 && i+1 < len(s.images) {
			im.limit = s.images[i+1].load
		}
	}
	return s
}

func (s *symbolizer) imageFor(pc uint64) *image {
	i := sort.Search(len(s.images), func(i int) bool { return s.images[i].load > pc }) - 1
	if i < 0 {
		return nil
	}
	im := s.images[i]
	if im.limit != 0 && pc >= im.limit {
		return nil
	}
	return im
}

// frames resolves a pc. return addresses (leaf=false) point after the call, so they are looked up at
// pc-1 to land on the call instruction itself.
func (s *symbolizer) frames(pc uint64, leaf bool) []frame {
	k := pcKey{pc, leaf}
	if fs, ok := s.cache[k]; ok {
		return fs
	}
	lookup := pc
	if !leaf {
		lookup--
	}
	var fs []frame
	if im := s.imageFor(pc); im != nil {
		if im.dw != nil {
			fs = im.dw.frames(lookup - im.slide)
		}
		if fs == nil {
			if name := im.names[pc]; name != "" {
				fs = []frame{{fn: name}}
			} else if name := im.nearestKnown(lookup); name != "" {
				fs = []frame{{fn: name}}
			} else {
				fs = []frame{{fn: fmt.Sprintf("%s+0x%x", filepath.Base(im.path), pc-im.load)}}
			}
		}
	} else {
		fs = []frame{{fn: fmt.Sprintf("0x%x", pc)}}
	}
	s.cache[k] = fs
	return fs
}

// nearestKnown names a pc after the closest address below it that Instruments symbolicated in the
// same image, if that address is near enough to plausibly be the same function.
func (im *image) nearestKnown(pc uint64) string {
	i := sort.Search(len(im.known), func(i int) bool { return im.known[i].addr > pc }) - 1
	if i < 0 || pc-im.known[i].addr > 2048 {
		return ""
	}
	return im.known[i].name
}

// resolveFallbacks batches pcs without DWARF through atos, one call per image. atos misnames
// functions in the dyld shared cache (it reports _platform_strchr for _platform_memmove), so system
// images rely on Instruments' own names instead.
func (s *symbolizer) resolveFallbacks(pcs map[uint64]bool) {
	byImage := map[*image][]uint64{}
	for pc := range pcs {
		if im := s.imageFor(pc); im != nil && !systemImage(im.path) && (im.dw == nil || im.dw.frames(pc-1-im.slide) == nil) {
			byImage[im] = append(byImage[im], pc)
		}
	}
	for im, list := range byImage {
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
		args := []string{"-o", im.path, "-l", fmt.Sprintf("0x%x", im.load)}
		for _, pc := range list {
			args = append(args, fmt.Sprintf("0x%x", pc))
		}
		out, err := exec.Command("atos", args...).Output()
		if err != nil {
			continue
		}
		im.names = map[uint64]string{}
		for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if i >= len(list) {
				break
			}
			name := strings.SplitN(line, " (in ", 2)[0] // "fn (in image) + 12"
			if !strings.HasPrefix(name, "0x") {
				im.names[list[i]] = name
			}
		}
	}
}

// openDWARF prefers the binary's own DWARF (go), then a .dSYM next to it, creating one with
// dsymutil when the binary only references its object files (zig, c, rust).
func openDWARF(path string, f *macho.File) *dwarfIndex {
	if d, err := f.DWARF(); err == nil {
		if ix := indexDWARF(d); ix != nil {
			return ix
		}
	}
	dsym := filepath.Join(path+".dSYM", "Contents", "Resources", "DWARF", filepath.Base(path))
	if _, err := os.Stat(dsym); err != nil {
		if systemImage(path) {
			return nil
		}
		if exec.Command("dsymutil", path).Run() != nil {
			return nil
		}
	}
	df, err := macho.Open(dsym)
	if err != nil {
		return nil
	}
	defer df.Close()
	d, err := df.DWARF()
	if err != nil {
		return nil
	}
	return indexDWARF(d)
}

// scope is a function body or one inlined call site within it.
type scope struct {
	ranges   [][2]uint64
	depth    int
	name     string
	callFile string
	callLine int
}

type function struct {
	scope
	cu      *dwarf.Entry
	inlined []scope
}

type dwarfIndex struct {
	d     *dwarf.Data
	funcs []*function // sorted by first range start
	spans []span      // every function range, sorted, for lookup
	lines map[dwarf.Offset]*dwarf.LineReader
}

type span struct {
	lo, hi uint64
	fn     *function
}

func indexDWARF(d *dwarf.Data) *dwarfIndex {
	ix := &dwarfIndex{d: d, lines: map[dwarf.Offset]*dwarf.LineReader{}}
	r := d.Reader()
	var cu *dwarf.Entry
	var files []*dwarf.LineFile
	var current *function
	currentDepth := -1
	depth := 0
	for {
		e, err := r.Next()
		if err != nil || e == nil {
			break
		}
		if e.Tag == 0 {
			depth--
			if depth <= currentDepth {
				current, currentDepth = nil, -1
			}
			continue
		}
		switch e.Tag {
		case dwarf.TagCompileUnit:
			cu, files = e, nil
			if lr, err := d.LineReader(e); err == nil && lr != nil {
				ix.lines[e.Offset] = lr
				files = lr.Files()
			}
		case dwarf.TagSubprogram:
			if current == nil {
				ranges, _ := d.Ranges(e)
				if len(ranges) > 0 {
					current = &function{scope: scope{ranges: ranges, depth: depth, name: ix.name(e)}, cu: cu}
					currentDepth = depth
					ix.funcs = append(ix.funcs, current)
				}
			}
		case dwarf.TagInlinedSubroutine:
			if current != nil {
				ranges, _ := d.Ranges(e)
				if len(ranges) > 0 {
					sc := scope{ranges: ranges, depth: depth, name: ix.name(e)}
					if idx, ok := e.Val(dwarf.AttrCallFile).(int64); ok && idx >= 0 && int(idx) < len(files) && files[idx] != nil {
						sc.callFile = files[idx].Name
					}
					if l, ok := e.Val(dwarf.AttrCallLine).(int64); ok {
						sc.callLine = int(l)
					}
					current.inlined = append(current.inlined, sc)
				}
			}
		}
		if e.Children {
			depth++
		}
	}
	if len(ix.funcs) == 0 {
		return nil
	}
	for _, fn := range ix.funcs {
		for _, rg := range fn.ranges {
			ix.spans = append(ix.spans, span{rg[0], rg[1], fn})
		}
	}
	sort.Slice(ix.spans, func(i, j int) bool { return ix.spans[i].lo < ix.spans[j].lo })
	return ix
}

// name prefers the linkage name, which zig fills with the fully qualified name, unless it is a
// mangled C++ or Swift symbol; go's plain names are already qualified.
func (ix *dwarfIndex) name(e *dwarf.Entry) string {
	if ln, ok := e.Val(dwarf.AttrLinkageName).(string); ok && !strings.HasPrefix(ln, "_Z") && !strings.HasPrefix(ln, "$s") {
		return ln
	}
	if n, ok := e.Val(dwarf.AttrName).(string); ok {
		return n
	}
	for _, a := range []dwarf.Attr{dwarf.AttrAbstractOrigin, dwarf.AttrSpecification} {
		if off, ok := e.Val(a).(dwarf.Offset); ok {
			r := ix.d.Reader()
			r.Seek(off)
			if o, err := r.Next(); err == nil && o != nil {
				return ix.name(o)
			}
		}
	}
	return "?"
}

// frames returns the inline chain at a file address, innermost first. the innermost frame's line
// comes from the line table; each outer frame's line is the call site of the scope inlined into it.
func (ix *dwarfIndex) frames(addr uint64) []frame {
	// function ranges don't overlap, so the last span starting at or before addr is the only candidate
	i := sort.Search(len(ix.spans), func(i int) bool { return ix.spans[i].lo > addr }) - 1
	if i < 0 || addr >= ix.spans[i].hi {
		return nil
	}
	fn := ix.spans[i].fn

	var chain []scope // enclosing inline scopes, outermost first
	for _, sc := range fn.inlined {
		for _, rg := range sc.ranges {
			if addr >= rg[0] && addr < rg[1] {
				chain = append(chain, sc)
				break
			}
		}
	}
	sort.SliceStable(chain, func(a, b int) bool { return chain[a].depth < chain[b].depth })

	file, line := ix.lineAt(fn.cu, addr)
	names := append([]string{fn.name}, scopeNames(chain)...)
	out := []frame{{fn: names[len(names)-1], file: file, line: line}}
	for k := len(chain) - 1; k >= 0; k-- {
		out = append(out, frame{fn: names[k], file: chain[k].callFile, line: chain[k].callLine})
	}
	return out
}

func scopeNames(chain []scope) []string {
	out := make([]string, len(chain))
	for i, sc := range chain {
		out[i] = sc.name
	}
	return out
}

func (ix *dwarfIndex) lineAt(cu *dwarf.Entry, addr uint64) (string, int) {
	if cu == nil {
		return "", 0
	}
	lr := ix.lines[cu.Offset]
	if lr == nil {
		return "", 0
	}
	var le dwarf.LineEntry
	if err := lr.SeekPC(addr, &le); err != nil || le.File == nil {
		return "", 0
	}
	return le.File.Name, le.Line
}

func systemImage(path string) bool {
	return strings.HasPrefix(path, "/System/") || strings.HasPrefix(path, "/usr/lib/")
}
