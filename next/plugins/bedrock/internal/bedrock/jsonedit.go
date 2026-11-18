package bedrock

import (
	"cmp"
	"errors"
	"slices"
	"unsafe"

	"github.com/tidwall/gjson"
)

// edits are changes to one JSON document, located by the byte offsets
// gjson reports in the original and applied in one pass (bytes). What no
// edit touches is copied byte for byte: unknown fields, key order, number
// formatting, whitespace. The result is one allocation of the final size
// however many edits there are, where a chain of sjson calls would copy the
// whole document once per edit.
type edits struct {
	src  []byte
	list []edit
}

// edit replaces src[start:end] with with; start == end inserts.
type edit struct {
	start, end int
	with       []byte
}

// member is an object member to set: a plain key (written without
// escaping) and its value as raw JSON.
type member struct {
	key string
	raw []byte
}

var (
	errNotObject = errors.New("the request body is not a JSON object")
	errOverlap   = errors.New("overlapping body edits")
)

// parseObject reads body, a JSON object, without copying it: the document
// is only read, and bytes builds a new slice.
func parseObject(body []byte) (gjson.Result, *edits, error) {
	if !gjson.ValidBytes(body) {
		return gjson.Result{}, nil, errNotObject
	}
	doc := gjson.Parse(unsafe.String(unsafe.SliceData(body), len(body)))
	if !doc.IsObject() {
		return gjson.Result{}, nil, errNotObject
	}
	return doc, &edits{src: body}, nil
}

// replace sets v, a value read from the document, to raw.
func (e *edits) replace(v gjson.Result, raw []byte) {
	e.list = append(e.list, edit{v.Index, v.Index + len(v.Raw), raw})
}

// object rewrites the members of obj, an object read from the document
// (anything else is left alone). A member drop reports is removed (drop may
// be nil). The first member named in set takes the value given there and
// later members with that key are removed; a key obj lacks is appended, in
// the order of set.
func (e *edits) object(obj gjson.Result, drop func(key string, value gjson.Result) bool, set ...member) {
	if !obj.IsObject() {
		return
	}
	found := make([]bool, len(set))
	keptEnd := -1        // the end of the last member kept, -1 before the first
	cut, cutEnd := -1, 0 // the run of members removed since then; cut -1: none
	obj.ForEach(func(k, v gjson.Result) bool {
		i := slices.IndexFunc(set, func(m member) bool { return m.key == k.Str })
		if (drop != nil && drop(k.Str, v)) || (i >= 0 && found[i]) {
			if cut < 0 {
				cut = k.Index
				if keptEnd >= 0 {
					cut = keptEnd // after a kept member: with the comma before it
				}
			}
			cutEnd = v.Index + len(v.Raw)
			return true
		}
		if cut >= 0 {
			end := cutEnd
			if keptEnd < 0 {
				end = k.Index // a leading run: with the comma after it
			}
			e.list = append(e.list, edit{start: cut, end: end})
			cut = -1
		}
		if i >= 0 {
			found[i] = true
			e.replace(v, set[i].raw)
		}
		keptEnd = v.Index + len(v.Raw)
		return true
	})
	if cut >= 0 {
		e.list = append(e.list, edit{start: cut, end: cutEnd})
	}
	e.appendMembers(obj, keptEnd, set, found)
}

// appendMembers inserts the members of set not found after the last kept
// member of obj (keptEnd), or at its start when none is kept.
func (e *edits) appendMembers(obj gjson.Result, keptEnd int, set []member, found []bool) {
	var add []byte
	for i, m := range set {
		if found[i] {
			continue
		}
		if keptEnd >= 0 || len(add) > 0 {
			add = append(add, ',')
		}
		add = append(append(append(append(add, '"'), m.key...), '"', ':'), m.raw...)
	}
	if add == nil {
		return
	}
	at := keptEnd
	if at < 0 {
		at = obj.Index + 1
	}
	e.list = append(e.list, edit{start: at, end: at, with: add})
}

// bytes applies the edits to a new slice.
func (e *edits) bytes() ([]byte, error) {
	slices.SortStableFunc(e.list, func(a, b edit) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end))
	})
	size := len(e.src)
	for _, d := range e.list {
		size += len(d.with) - (d.end - d.start)
	}
	out, at := make([]byte, 0, size), 0
	for _, d := range e.list {
		if d.start < at {
			return nil, errOverlap
		}
		out = append(append(out, e.src[at:d.start]...), d.with...)
		at = d.end
	}
	return append(out, e.src[at:]...), nil
}
