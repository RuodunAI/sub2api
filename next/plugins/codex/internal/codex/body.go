package codex

import (
	"cmp"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// The request body is edited where it needs a change and nowhere else: the
// edits name byte ranges of the body as received and are applied in one
// copy, so untouched members keep their bytes, key order and spacing, and a
// body of any size costs one parse and one output buffer.

// edit replaces body[start:end] with text (an insertion when start == end).
type edit struct {
	start, end int
	text       string
}

// replaceValue replaces the raw JSON of v.
func replaceValue(v gjson.Result, text string) edit {
	return edit{start: v.Index, end: v.Index + len(v.Raw), text: text}
}

// splice applies non-overlapping edits to body. Without edits it returns
// body itself.
func splice(body []byte, edits []edit) []byte {
	if len(edits) == 0 {
		return body
	}
	slices.SortStableFunc(edits, func(a, b edit) int { return cmp.Compare(a.start, b.start) })
	size := len(body)
	for _, e := range edits {
		size += len(e.text) - (e.end - e.start)
	}
	out := make([]byte, 0, size)
	at := 0
	for _, e := range edits {
		out = append(append(out, body[at:e.start]...), e.text...)
		at = e.end
	}
	return append(out, body[at:]...)
}

// member is one member of a JSON object; both results carry their offset
// in the body.
type member struct {
	key, value gjson.Result
}

func (m member) start() int { return m.key.Index }
func (m member) end() int   { return m.value.Index + len(m.value.Raw) }

// object is a JSON object of the body with its members in order.
type object struct {
	value   gjson.Result
	members []member
}

// parseObject reads the members of the top-level object of body; ok is false
// when body is not an object.
func parseObject(body []byte) (object, bool) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return object{}, false
	}
	root.Index = len(body) - len(root.Raw) // Parse drops leading space without counting it
	return object{value: root, members: membersOf(root)}, true
}

// membersOf lists the members of the object v.
func membersOf(v gjson.Result) []member {
	var out []member
	v.ForEach(func(k, val gjson.Result) bool {
		out = append(out, member{key: k, value: val})
		return true
	})
	return out
}

// reshape removes the members drop selects and appends the members added
// ("name":value pairs, in order) at the end of the object. A removed member
// takes its separating comma along; the bytes of every kept member and of
// the space before it stay as they are.
func (o object) reshape(drop func(member) bool, added ...string) []edit {
	ms := o.members
	first := slices.IndexFunc(ms, func(m member) bool { return !drop(m) })
	var edits []edit
	for i, m := range ms {
		if !drop(m) {
			continue
		}
		switch {
		case first >= 0 && i > first: // with the comma after the previous member
			edits = append(edits, edit{start: ms[i-1].end(), end: m.end()})
		case i+1 < len(ms): // before the first kept member: up to the next key
			edits = append(edits, edit{start: m.start(), end: ms[i+1].start()})
		default:
			edits = append(edits, edit{start: m.start(), end: m.end()})
		}
	}
	if len(added) == 0 {
		return edits
	}
	text := strings.Join(added, ",")
	at := o.value.Index + 1 // just after "{" of an empty object
	if len(ms) > 0 {
		at = ms[len(ms)-1].end()
	}
	if first >= 0 {
		text = "," + text
	}
	return append(edits, edit{start: at, end: at, text: text})
}
