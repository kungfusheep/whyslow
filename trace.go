package main

// trace.go reads the XML that `xctrace export` produces. exports deduplicate repeated values: the
// first occurrence of a value carries an id attribute and later ones are empty elements with a ref
// pointing back at it, so every lookup goes through resolve.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type node struct {
	tag   string
	attrs map[string]string
	text  strings.Builder
	kids  []*node
}

func (n *node) attr(k string) string { return n.attrs[k] }

func (n *node) child(tag string) *node {
	for _, k := range n.kids {
		if k.tag == tag {
			return k
		}
	}
	return nil
}

// value is the element's own text, falling back to its fmt attribute.
func (n *node) value() string {
	if s := strings.TrimSpace(n.text.String()); s != "" {
		return s
	}
	return n.attrs["fmt"]
}

func parseNodes(r io.Reader) (*node, error) {
	d := xml.NewDecoder(r)
	root := &node{tag: "#root"}
	stack := []*node{root}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return root, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{tag: t.Name.Local, attrs: make(map[string]string, len(t.Attr))}
			for _, a := range t.Attr {
				n.attrs[a.Name.Local] = a.Value
			}
			top := stack[len(stack)-1]
			top.kids = append(top.kids, n)
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			stack[len(stack)-1].text.Write(t)
		}
	}
}

// doc is one exported table (or the table of contents) with its id index.
type doc struct {
	root *node
	byID map[string]*node
}

func newDoc(root *node) *doc {
	d := &doc{root: root, byID: map[string]*node{}}
	var walk func(*node)
	walk = func(n *node) {
		if id := n.attrs["id"]; id != "" {
			d.byID[id] = n
		}
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(root)
	return d
}

func (d *doc) resolve(n *node) *node {
	if n == nil {
		return nil
	}
	if ref := n.attrs["ref"]; ref != "" {
		if t := d.byID[ref]; t != nil {
			return t
		}
	}
	return n
}

func (d *doc) find(tag string) []*node {
	var out []*node
	var walk func(*node)
	walk = func(n *node) {
		if n.tag == tag {
			out = append(out, n)
		}
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(d.root)
	return out
}

// table is a schema table: column mnemonics plus rows of resolved cells.
type table struct {
	*doc
	cols map[string]int
	rows [][]*node
}

func (t *table) cell(row []*node, col string) *node {
	i, ok := t.cols[col]
	if !ok || i >= len(row) {
		return nil
	}
	return row[i]
}

func exportTable(trace, schema string) (*table, error) {
	xpath := fmt.Sprintf(`/trace-toc/run[@number="1"]/data/table[@schema=%q]`, schema)
	root, err := xctraceExport(trace, "--xpath", xpath)
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", schema, err)
	}
	return newTable(root), nil
}

func newTable(root *node) *table {
	t := &table{doc: newDoc(root), cols: map[string]int{}}
	for i, c := range t.find("col") {
		if m := c.child("mnemonic"); m != nil {
			t.cols[m.value()] = i
		}
	}
	for _, r := range t.find("row") {
		cells := make([]*node, len(r.kids))
		for i, c := range r.kids {
			cells[i] = t.resolve(c)
		}
		t.rows = append(t.rows, cells)
	}
	return t
}

func exportTOC(trace string) (*doc, error) {
	root, err := xctraceExport(trace, "--toc")
	if err != nil {
		return nil, fmt.Errorf("export toc: %w", err)
	}
	return newDoc(root), nil
}

func xctraceExport(trace string, args ...string) (*node, error) {
	cmd := exec.Command("xctrace", append([]string{"export", "--input", trace}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseNodes(bytes.NewReader(out))
}

// launchedPID finds the process xctrace launched from the trace's table of contents.
func launchedPID(toc *doc) (int, string, error) {
	for _, p := range toc.find("process") {
		if p.attr("type") == "launched" {
			var pid int
			if _, err := fmt.Sscan(p.attr("pid"), &pid); err == nil {
				return pid, p.attr("name"), nil
			}
		}
	}
	return 0, "", fmt.Errorf("no launched process in trace")
}
