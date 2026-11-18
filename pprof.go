package main

// pprof.go writes a pprof profile (profile.proto), already symbolized with inline frames and source
// lines, with three sample types: cycles (timer samples with full stacks), l1d_misses and
// branch_mispredicts (event samples). `go tool pprof -http=:` then shows flame graphs and annotated
// source for each, switchable from the SAMPLE menu, for any language.
//
// event samples carry only the pc that caused them, so each borrows the most common call path of
// its function from the cycle samples. that keeps them in the right place in flame graphs; the leaf
// function and line are exact, the callers above it are the function's typical ones.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
)

func writePprof(path string, p *profile, evs []*events, durationNanos uint64) error {
	w := &pprofWriter{strings: map[string]int64{"": 0}, stringList: []string{""}, funcs: map[frame]uint64{}}

	var body pbuf
	types := []string{"cycles"}
	for _, ev := range evs {
		types = append(types, ev.sampleType)
	}
	for _, t := range types {
		var vt pbuf
		vt.int64Field(1, w.str(t))
		vt.int64Field(2, w.str("count"))
		body.bytesField(1, vt.b)
	}

	locIDs := map[*loc]uint64{}
	locID := func(l *loc) uint64 {
		id, ok := locIDs[l]
		if !ok {
			id = uint64(len(locIDs) + 1)
			locIDs[l] = id
			body.bytesField(4, w.location(id, l))
		}
		return id
	}
	sample := func(ids []uint64, typ int, v uint64) {
		vals := make([]uint64, len(types))
		vals[typ] = v
		var sp pbuf
		sp.packedField(1, ids)
		sp.packedField(2, vals)
		body.bytesField(2, sp.b)
	}

	callers := map[string][]*loc{} // leaf function -> its most common caller chain
	chainCount := map[string]int{}
	for _, smp := range p.samples {
		var ids []uint64
		var chain []*loc
		for i, l := range smp.locs {
			if l.lr && i > 0 && sameFunction(l, smp.locs[0]) {
				continue
			}
			ids = append(ids, locID(l))
			if i > 0 {
				chain = append(chain, l)
			}
		}
		sample(ids, 0, uint64(smp.delta[evCycles]))

		leaf := smp.locs[0].frames[0].fn
		key := leaf + fmt.Sprint(ids[1:])
		chainCount[key]++
		if best, ok := callers[leaf]; !ok || chainCount[key] > chainCount[leaf+fmt.Sprint(idsOf(locIDs, best))] {
			callers[leaf] = chain
		}
	}

	for ti, ev := range evs {
		for _, l := range ev.locs {
			ids := []uint64{locID(l)}
			for _, c := range callers[l.frames[0].fn] {
				ids = append(ids, locID(c))
			}
			sample(ids, ti+1, 1)
		}
	}

	for _, fn := range w.funcList {
		body.bytesField(5, fn)
	}
	var period pbuf
	period.int64Field(1, w.str("cycles"))
	period.int64Field(2, w.str("count"))
	body.int64Field(10, int64(durationNanos))
	body.bytesField(11, period.b)
	body.int64Field(12, 1)
	body.int64Field(14, w.str("cycles")) // default_sample_type
	for _, s := range w.stringList {
		body.bytesField(6, []byte(s))
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(body.b); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, gz.Bytes(), 0o644)
}

func idsOf(locIDs map[*loc]uint64, chain []*loc) []uint64 {
	out := make([]uint64, len(chain))
	for i, l := range chain {
		out[i] = locIDs[l]
	}
	return out
}

type pprofWriter struct {
	strings    map[string]int64
	stringList []string
	funcs      map[frame]uint64
	funcList   [][]byte
}

func (w *pprofWriter) str(s string) int64 {
	if i, ok := w.strings[s]; ok {
		return i
	}
	i := int64(len(w.stringList))
	w.strings[s] = i
	w.stringList = append(w.stringList, s)
	return i
}

// location encodes one address with its inline chain; pprof wants the innermost function first.
func (w *pprofWriter) location(id uint64, l *loc) []byte {
	var b pbuf
	b.uint64Field(1, id)
	b.uint64Field(3, l.pc)
	for _, f := range l.frames {
		var line pbuf
		line.uint64Field(1, w.function(f))
		line.int64Field(2, int64(f.line))
		b.bytesField(4, line.b)
	}
	return b.b
}

func (w *pprofWriter) function(f frame) uint64 {
	k := frame{fn: f.fn, file: f.file}
	if id, ok := w.funcs[k]; ok {
		return id
	}
	id := uint64(len(w.funcs) + 1)
	w.funcs[k] = id
	var b pbuf
	b.uint64Field(1, id)
	b.int64Field(2, w.str(f.fn))
	b.int64Field(3, w.str(f.fn))
	b.int64Field(4, w.str(f.file))
	w.funcList = append(w.funcList, b.b)
	return id
}

// pbuf is a minimal protobuf encoder: varints, length-delimited fields and packed repeated varints.
type pbuf struct{ b []byte }

func (p *pbuf) varint(x uint64) {
	for x >= 0x80 {
		p.b = append(p.b, byte(x)|0x80)
		x >>= 7
	}
	p.b = append(p.b, byte(x))
}

func (p *pbuf) key(field, wire int) { p.varint(uint64(field<<3 | wire)) }

func (p *pbuf) uint64Field(field int, v uint64) {
	if v == 0 {
		return
	}
	p.key(field, 0)
	p.varint(v)
}

func (p *pbuf) int64Field(field int, v int64) { p.uint64Field(field, uint64(v)) }

func (p *pbuf) bytesField(field int, b []byte) {
	p.key(field, 2)
	p.varint(uint64(len(b)))
	p.b = append(p.b, b...)
}

func (p *pbuf) packedField(field int, vs []uint64) {
	var inner pbuf
	for _, v := range vs {
		inner.varint(v)
	}
	p.bytesField(field, inner.b)
}
