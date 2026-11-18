package main

// detect.go finds what can be profiled in a project directory: go benchmarks (read from the test
// sources, so nothing is compiled), zig executables in zig-out/bin, and named targets from an
// optional .whyslow file, for anything detection can't see (like a benchmark binary's -filter).

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type candidate struct {
	Kind  string // "go", "zig" or "file" (from .whyslow)
	Name  string
	Where string   // package dir for go, the command for everything else
	bench string   // go: the benchmark function
	argv  []string // native: the command to profile
}

func detect(root string) []candidate {
	var out []candidate
	out = append(out, whyslowFile(root)...)
	out = append(out, goBenchmarks(root)...)
	out = append(out, zigExecutables(root)...)
	return out
}

// whyslowFile reads "name = command args..." lines; # starts a comment.
func whyslowFile(root string) []candidate {
	f, err := os.Open(filepath.Join(root, ".whyslow"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []candidate
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, cmd, ok := strings.Cut(line, "=")
		argv := strings.Fields(cmd)
		if !ok || len(argv) == 0 {
			continue
		}
		out = append(out, candidate{Kind: "file", Name: strings.TrimSpace(name), Where: strings.Join(argv, " "), argv: argv})
	}
	return out
}

// goBenchmarks parses every _test.go file under root for func BenchmarkX(b *testing.B).
// sub-benchmarks created with b.Run aren't visible this way; a .whyslow entry can name those.
func goBenchmarks(root string) []candidate {
	var out []candidate
	fset := token.NewFileSet()
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pkg := "./" + filepath.ToSlash(rel)
		if rel == "." {
			pkg = "."
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && isBenchmark(fn) {
				out = append(out, candidate{Kind: "go", Name: fn.Name.Name, Where: pkg, bench: fn.Name.Name})
			}
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func isBenchmark(fn *ast.FuncDecl) bool {
	rest, ok := strings.CutPrefix(fn.Name.Name, "Benchmark")
	if !ok || (rest != "" && strings.ToLower(rest[:1]) == rest[:1]) {
		return false // go test ignores BenchmarkfooBar
	}
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "B"
}

func zigExecutables(root string) []candidate {
	if _, err := os.Stat(filepath.Join(root, "build.zig")); err != nil {
		return nil
	}
	bin := filepath.Join(root, "zig-out", "bin")
	entries, _ := os.ReadDir(bin)
	var out []candidate
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		path := filepath.Join(bin, e.Name())
		out = append(out, candidate{Kind: "zig", Name: e.Name(), Where: filepath.Join("zig-out", "bin", e.Name()), argv: []string{path}})
	}
	return out
}
