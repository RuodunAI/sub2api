package jsonedit

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// object holds the member edits of one object: of the document (v, start)
// or a new one built by Set (start -1). They are resolved by render in one
// walk over the members, so removed runs and added members get exactly one
// comma between neighbours whatever the combination.
type object struct {
	v     gjson.Result
	start int // offset of the "{"
	drops []func(key string, value gjson.Result) bool
	sets  []*member
}

// member is a member Set or Default writes: raw, or a new object (sub)
// when the path goes on below a member that is missing.
type member struct {
	key  string
	raw  []byte
	sub  *object
	keep bool // Default: only where the member is missing
}

// object returns the edits of v, an object of the document. For a value
// that is not an object it returns nil, failing when must is set.
func (d *Doc) object(v gjson.Result, must bool) *object {
	start, ok := d.offset(v.Raw)
	switch {
	case !ok && v.Exists():
		d.fail(ErrForeign)
		return nil
	case !v.IsObject():
		if must {
			d.fail(ErrNotObject)
		}
		return nil
	}
	if o := d.byStart[start]; o != nil {
		return o
	}
	if d.byStart == nil {
		d.byStart = map[int]*object{}
	}
	o := &object{v: v, start: start}
	d.byStart[start] = o
	d.objects = append(d.objects, o)
	return o
}

// put records keys (one or more) to be set to raw in o.
func (o *object) put(keys []string, raw []byte, keep bool) {
	i := slices.IndexFunc(o.sets, func(m *member) bool { return m.key == keys[0] })
	if i < 0 {
		o.sets = append(o.sets, &member{key: keys[0], keep: keep})
		i = len(o.sets) - 1
	} else if keep {
		if len(keys) == 1 || o.sets[i].sub == nil {
			return // already set: Default adds nothing
		}
	}
	m := o.sets[i]
	if !keep {
		m.keep = false
	}
	if len(keys) == 1 {
		m.raw, m.sub = raw, nil
		return
	}
	if m.sub == nil {
		m.raw, m.sub = nil, &object{start: -1}
	}
	m.sub.put(keys[1:], raw, keep)
}

// dropped reports a member some delete removes.
func (o *object) dropped(key string, value gjson.Result) bool {
	return slices.ContainsFunc(o.drops, func(drop func(string, gjson.Result) bool) bool { return drop(key, value) })
}

// render appends the spans of o's edits: removed members go with one
// separating comma (the one before them, or after a leading run the one
// after), set members replace their value in place, and the members still
// missing are added before the "}" when the last member stays (as sjson
// adds them), else where the last removed member ended (the space that
// followed it stays before the "}"), or after the "{" of an empty object.
func (d *Doc) render(o *object, spans []span) []span {
	found := make([]bool, len(o.sets))
	end := o.start + 1   // where added members go
	keptEnd := -1        // end of the last kept member, -1 before the first
	cut, cutEnd := -1, 0 // the run of removed members since then; cut -1: none
	o.v.ForEach(func(k, v gjson.Result) bool {
		keyStart, _ := d.offset(k.Raw)
		valueStart, _ := d.offset(v.Raw)
		end = valueStart + len(v.Raw)
		if o.dropped(k.Str, v) {
			if cut < 0 {
				cut = keyStart
				if keptEnd >= 0 {
					cut = keptEnd // with the comma before it
				}
			}
			cutEnd = end
			return true
		}
		if cut >= 0 {
			to := cutEnd
			if keptEnd < 0 {
				to = keyStart // a leading run: with the comma after it
			}
			spans, cut = append(spans, span{start: cut, end: to}), -1
		}
		keptEnd = end
		i := slices.IndexFunc(o.sets, func(m *member) bool { return m.key == k.Str })
		if i < 0 {
			return true
		}
		m := o.sets[i]
		if found[i] && !m.keep {
			d.fail(fmt.Errorf("%w: %q", ErrDuplicateKey, k.Str))
			return false
		}
		found[i] = true
		if m.keep {
			return true
		}
		if text := m.value(); v.Raw != string(text) {
			spans = append(spans, span{valueStart, end, text})
		}
		return true
	})
	if cut >= 0 {
		spans = append(spans, span{start: cut, end: cutEnd})
	}
	var add []byte
	for i, m := range o.sets {
		if found[i] {
			continue
		}
		if keptEnd >= 0 || len(add) > 0 {
			add = append(add, ',')
		}
		add = m.appendTo(add)
	}
	if add != nil {
		if keptEnd == end { // after a kept last member: before the "}", as sjson does
			end = o.start + strings.LastIndexByte(o.v.Raw, '}')
		}
		spans = append(spans, span{start: end, end: end, text: add})
	}
	return spans
}

// appendTo appends m as "key":value.
func (m *member) appendTo(dst []byte) []byte {
	return append(append(gjson.AppendJSONString(dst, m.key), ':'), m.value()...)
}

// value is the value of m: raw, or the new object.
func (m *member) value() []byte {
	if m.sub == nil {
		return m.raw
	}
	dst := []byte{'{'}
	for i, s := range m.sub.sets {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = s.appendTo(dst)
	}
	return append(dst, '}')
}
