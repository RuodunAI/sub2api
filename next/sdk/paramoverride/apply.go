package paramoverride

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Context are the request facts the conditions of an override
// can read next to the body (CONTRACTS §84).
type Context struct {
	// Model is the upstream model after the account's model mapping;
	// OriginalModel the client's.
	Model, OriginalModel string
	RequestPath          string
	Headers              http.Header // client request headers
	RetryIndex           int         // 0 for the first attempt
	UserID, AccountID    int64
	Group                string
	Stream               bool
	// InitialHeaders is what the account's header override (CONTRACTS
	// §108) decided for this request: the run starts from it, so its
	// conditions, keep_origin and header reads see it, and the result
	// includes what the run leaves of it.
	InitialHeaders HeaderOverride
}

// document is the context object conditions read: the keys new-api offers
// a channel's override (group also as using_group), plus account_id and
// stream.
func (in *Context) document(headerOverride map[string]string) []byte {
	if in == nil {
		in = &Context{}
	}
	headers := map[string]string{}
	for name, values := range in.Headers {
		lower := strings.ToLower(name)
		if v := strings.TrimSpace(strings.Join(values, ", ")); v != "" && !HeaderBlocked(lower) {
			headers[lower] = v
		}
	}
	retry := map[string]any{"index": in.RetryIndex, "is_retry": in.RetryIndex > 0}
	doc, _ := json.Marshal(map[string]any{
		"model": firstText(in.Model, in.OriginalModel), "upstream_model": in.Model, "original_model": in.OriginalModel,
		"request_path": in.RequestPath, "request_headers": headers, "header_override": headerOverride,
		"retry_index": in.RetryIndex, "is_retry": in.RetryIndex > 0, "retry": retry,
		"user_id": in.UserID, "group": in.Group, "using_group": in.Group, "account_id": in.AccountID, "stream": in.Stream,
	})
	return doc
}

// HeaderOverride is what the header operations decided: headers to set
// (lower-case name → value) and to remove from the upstream request.
type HeaderOverride struct {
	Set    map[string]string
	Delete []string
}

// Apply sets and removes the headers on h.
func (o HeaderOverride) Apply(h http.Header) {
	for name, v := range o.Set {
		h.Set(name, v)
	}
	for _, name := range o.Delete {
		h.Del(name)
	}
}

// Result is the overridden body and the header decisions.
type Result struct {
	Body    []byte
	Headers HeaderOverride
}

// Apply runs the override on a JSON request body. A return_error operation
// ends it with a *Error; any other error is an operation that
// could not run on this body (a type mismatch, a missing source). A nil
// override returns the body unchanged and the context's header override.
func (p *Override) Apply(body []byte, in *Context) (Result, error) {
	if p == nil {
		out := Result{Body: body}
		if in != nil {
			out.Headers = in.InitialHeaders
		}
		return out, nil
	}
	if p.broken != "" {
		return Result{}, errors.New(p.broken)
	}
	r := newRun(body, in)
	for _, s := range p.legacy {
		if err := r.setRaw(s.path, s.raw); err != nil {
			return Result{}, fmt.Errorf("set %s: %w", s.path, err)
		}
	}
	for i := range p.ops {
		op := &p.ops[i]
		ok, err := r.matches(r.body, op.conds, op.all)
		if err == nil && ok {
			err = op.run(r)
		}
		var returned *Error
		if errors.As(err, &returned) {
			return Result{}, returned
		}
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", op.label, err)
		}
	}
	return Result{Body: r.body, Headers: r.headerResult()}, nil
}

// overrideRun is one run of an override over a body.
type overrideRun struct {
	body []byte
	in   *Context
	// ctx is the context document, built when a condition first needs it
	// and again after a header operation changed header_override.
	ctx     []byte
	set     map[string]string
	deleted map[string]bool
}

// newRun starts a run from the context's header override.
func newRun(body []byte, in *Context) *overrideRun {
	r := &overrideRun{body: body, in: in, set: map[string]string{}, deleted: map[string]bool{}}
	if in == nil {
		return r
	}
	for name, v := range in.InitialHeaders.Set {
		r.set[name] = v
	}
	for _, name := range in.InitialHeaders.Delete {
		r.deleted[name] = true
	}
	return r
}

func (r *overrideRun) setRaw(path, raw string) (err error) {
	r.body, err = sjson.SetRawBytes(r.body, path, []byte(raw))
	return err
}

func (r *overrideRun) setString(path, s string) (err error) {
	r.body, err = sjson.SetBytes(r.body, path, s)
	return err
}

// ---- conditions

// matches evaluates conditions on data (the body, or an object
// prune_objects checks): all of them, or any.
func (r *overrideRun) matches(data []byte, conds []paramCond, all bool) (bool, error) {
	for _, c := range conds {
		ok, err := r.check(data, c)
		if err != nil || ok != all {
			return ok, err // OR: the first match; AND: the first miss
		}
	}
	return len(conds) == 0 || all, nil
}

// check evaluates a condition on data, or on the context when data has no
// such path.
func (r *overrideRun) check(data []byte, c paramCond) (bool, error) {
	v := gjson.GetBytes(data, resolvePaths(data, c.path, false)[0])
	if !v.Exists() {
		if r.ctx == nil {
			r.ctx = r.in.document(r.set)
		}
		v = gjson.GetBytes(r.ctx, c.path)
	}
	if !v.Exists() {
		return c.passMissing, nil
	}
	ok, err := c.compare(v)
	if err != nil {
		return false, fmt.Errorf("condition on %s: %w", c.path, err)
	}
	return ok != c.invert, nil
}

func (c paramCond) compare(v gjson.Result) (bool, error) {
	want, mode := c.value, c.mode
	switch mode {
	case "full":
		return conditionEqual(v, want)
	case "prefix":
		return strings.HasPrefix(v.String(), want.String()), nil
	case "suffix":
		return strings.HasSuffix(v.String(), want.String()), nil
	case "contains":
		return strings.Contains(v.String(), want.String()), nil
	case "regex":
		return c.re.MatchString(v.String()), nil
	}
	if v.Type != gjson.Number || want.Type != gjson.Number {
		return false, fmt.Errorf("%s compares numbers, got %s and %s", mode, kindOf(v), kindOf(want))
	}
	switch mode {
	case "gt":
		return v.Num > want.Num, nil
	case "gte":
		return v.Num >= want.Num, nil
	case "lt":
		return v.Num < want.Num, nil
	}
	return v.Num <= want.Num, nil // lte
}

// conditionEqual compares like new-api: null equals only null, booleans
// compare as booleans, other values of different types are an error.
func conditionEqual(v, want gjson.Result) (bool, error) {
	isBool := func(r gjson.Result) bool { return r.Type == gjson.True || r.Type == gjson.False }
	switch {
	case v.Type == gjson.Null || want.Type == gjson.Null:
		return v.Type == want.Type, nil
	case isBool(v) && isBool(want):
		return v.Type == want.Type, nil
	case v.Type != want.Type:
		return false, fmt.Errorf("cannot compare %s with %s", kindOf(v), kindOf(want))
	case v.Type == gjson.Number:
		return v.Num == want.Num, nil
	}
	return v.String() == want.String(), nil
}

func kindOf(v gjson.Result) string {
	switch {
	case !v.Exists():
		return "missing"
	case v.IsObject():
		return "an object"
	case v.IsArray():
		return "an array"
	case v.Type == gjson.String:
		return "a string"
	case v.Type == gjson.Number:
		return "a number"
	case v.Type == gjson.Null:
		return "null"
	}
	return "a boolean"
}

// ---- paths

// eachPath runs edit on every concrete path the operation's path names.
func eachPath(path string, edit func(*overrideRun, string) error) func(*overrideRun) error {
	return func(r *overrideRun) error {
		for _, p := range resolvePaths(r.body, path, true) {
			if err := edit(r, p); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		return nil
	}
}

// resolvePaths turns an operation path into concrete gjson/sjson paths:
// a negative array index counts from the end (messages.-1.content), and with
// wildcard a "*" segment expands over the elements of an array or the keys
// of an object. Past a wildcard only paths whose parent exists are kept.
// Paths with neither are returned as they are.
func resolvePaths(data []byte, path string, wildcard bool) []string {
	segs := splitPath(path)
	resolve := false
	for _, s := range segs {
		resolve = resolve || negativeIndex(s) || (wildcard && s == "*")
	}
	if !resolve {
		return []string{path}
	}
	paths, expanded := []string{""}, false
	for i, seg := range segs {
		last := i == len(segs)-1
		var next []string
		for _, prefix := range paths {
			node := nodeAt(data, prefix)
			switch {
			case wildcard && seg == "*":
				next = append(next, childPaths(node, prefix)...)
			case negativeIndex(seg) && node.IsArray():
				n, _ := strconv.Atoi(seg)
				index := seg
				if n += len(node.Array()); n >= 0 {
					index = strconv.Itoa(n)
				}
				next = append(next, joinPath(prefix, index))
			case expanded && !(last && node.IsObject()) && !nodeAt(data, joinPath(prefix, seg)).Exists():
				// past a wildcard: no such element or member
			default:
				next = append(next, joinPath(prefix, seg))
			}
		}
		paths, expanded = next, expanded || (wildcard && seg == "*")
	}
	return paths
}

// childPaths lists the paths of the elements or members of node.
func childPaths(node gjson.Result, prefix string) []string {
	var out []string
	eachChild(node, func(seg string, _ gjson.Result) bool {
		out = append(out, joinPath(prefix, seg))
		return true
	})
	return out
}

// eachChild calls fn with the path segment and the value of each element of
// an array or member of an object, until fn returns false; other nodes have
// none.
func eachChild(node gjson.Result, fn func(seg string, child gjson.Result) bool) {
	if !node.IsArray() && !node.IsObject() {
		return
	}
	i := 0
	node.ForEach(func(k, child gjson.Result) bool {
		seg := strconv.Itoa(i)
		if node.IsObject() {
			seg = escapePathKey(k.Str)
		}
		i++
		return fn(seg, child)
	})
}

func nodeAt(data []byte, path string) gjson.Result {
	if path == "" {
		return gjson.ParseBytes(data)
	}
	return gjson.GetBytes(data, path)
}

func joinPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

// splitPath splits a path at its unescaped dots.
func splitPath(path string) []string {
	var segs []string
	start := 0
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '\\':
			i++
		case '.':
			segs = append(segs, path[start:i])
			start = i + 1
		}
	}
	return append(segs, path[start:])
}

func negativeIndex(seg string) bool {
	if len(seg) < 2 || seg[0] != '-' {
		return false
	}
	for i := 1; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return false
		}
	}
	return true
}

// ---- body operations

// pathEdit is the edit of set, delete, prepend and append on one path.
func pathEdit(mode string, value gjson.Result, keep bool) func(*overrideRun, string) error {
	switch mode {
	case "set":
		return func(r *overrideRun, p string) error {
			if keep && gjson.GetBytes(r.body, p).Exists() {
				return nil
			}
			return r.setRaw(p, value.Raw)
		}
	case "delete":
		return func(r *overrideRun, p string) (err error) {
			r.body, err = sjson.DeleteBytes(r.body, p)
			return err
		}
	}
	prepend := mode == "prepend"
	return func(r *overrideRun, p string) error { return r.affix(p, value, prepend, keep) }
}

func (r *overrideRun) editString(p string, edit func(string) string) error {
	cur := gjson.GetBytes(r.body, p)
	if cur.Type != gjson.String {
		return fmt.Errorf("value is %s, not a string", kindOf(cur))
	}
	return r.setString(p, edit(cur.Str))
}

// affix prepends or appends value: elements to an array (an array value
// adds each of its elements), text to a string, members to an object
// (keep_origin keeps the members that are set).
func (r *overrideRun) affix(p string, value gjson.Result, prepend, keep bool) error {
	cur := gjson.GetBytes(r.body, p)
	switch {
	case cur.IsArray():
		add := []gjson.Result{value}
		if value.IsArray() {
			add = value.Array()
		}
		items := cur.Array()
		if prepend {
			items = append(add, items...)
		} else {
			items = append(items, add...)
		}
		raws := make([]string, len(items))
		for i, item := range items {
			raws[i] = item.Raw
		}
		return r.setRaw(p, "["+strings.Join(raws, ",")+"]")
	case cur.Type == gjson.String:
		if prepend {
			return r.setString(p, value.String()+cur.Str)
		}
		return r.setString(p, cur.Str+value.String())
	case cur.IsObject():
		return r.merge(p, cur, value, keep)
	}
	return fmt.Errorf("cannot prepend or append to %s", kindOf(cur))
}

func (r *overrideRun) merge(p string, cur, value gjson.Result, keep bool) error {
	if !value.IsObject() {
		return fmt.Errorf("only an object merges into an object, got %s", kindOf(value))
	}
	var err error
	value.ForEach(func(k, v gjson.Result) bool {
		key := escapePathKey(k.Str)
		if old := cur.Get(key); keep && old.Exists() && old.Type != gjson.Null {
			return true
		}
		err = r.setRaw(p+"."+key, v.Raw)
		return err == nil
	})
	return err
}

// copyValue copies (or moves) the value at from to to.
func (r *overrideRun) copyValue(from, to string, move bool) error {
	from, to = resolvePaths(r.body, from, false)[0], resolvePaths(r.body, to, false)[0]
	src := gjson.GetBytes(r.body, from)
	if !src.Exists() {
		return fmt.Errorf("source path %s does not exist", from)
	}
	if err := r.setRaw(to, src.Raw); err != nil {
		return err
	}
	if move {
		var err error
		r.body, err = sjson.DeleteBytes(r.body, from)
		return err
	}
	return nil
}

// syncTarget is a side of sync_fields: a JSON path of the body, or a header.
type syncTarget struct {
	key    string
	header bool
}

// syncFields copies the side that has a value to the side that has none;
// with both or neither it does nothing.
func (r *overrideRun) syncFields(from, to syncTarget) error {
	src, srcOK := r.syncValue(from)
	dst, dstOK := r.syncValue(to)
	switch {
	case srcOK && !dstOK:
		return r.syncWrite(to, src)
	case dstOK && !srcOK:
		return r.syncWrite(from, dst)
	}
	return nil
}

// syncValue reads a side; null and blank strings count as no value.
func (r *overrideRun) syncValue(t syncTarget) (gjson.Result, bool) {
	if t.header {
		v, ok := r.header(t.key)
		return gjson.Result{Type: gjson.String, Str: v}, ok
	}
	v := gjson.GetBytes(r.body, resolvePaths(r.body, t.key, false)[0])
	ok := v.Exists() && v.Type != gjson.Null && (v.Type != gjson.String || strings.TrimSpace(v.Str) != "")
	return v, ok
}

func (r *overrideRun) syncWrite(t syncTarget, v gjson.Result) error {
	switch {
	case t.header:
		return r.setHeader(t.key, strings.TrimSpace(v.String()))
	case v.Raw == "":
		return r.setString(resolvePaths(r.body, t.key, false)[0], v.Str)
	}
	return r.setRaw(resolvePaths(r.body, t.key, false)[0], v.Raw)
}

// pruneSpec is the value of prune_objects.
type pruneSpec struct {
	conds          []paramCond
	all, recursive bool
}

// pruned is an object prune_objects removes: a member of an object (every
// copy of its key goes, as in new-api) or an element of an array.
type pruned struct {
	path   string
	member bool
}

// prune removes the objects under each path the operation's path names
// (the whole body when empty) that meet the conditions; the objects at
// those paths themselves stay.
func (r *overrideRun) prune(path string, spec pruneSpec) error {
	var drop []pruned
	for _, p := range resolvePaths(r.body, path, true) {
		if err := r.collectPruned(nodeAt(r.body, p), p, spec, &drop); err != nil {
			return err
		}
	}
	// From the back, so the indexes of earlier elements stay valid.
	for i := len(drop) - 1; i >= 0; i-- {
		if err := r.deletePruned(drop[i]); err != nil {
			return err
		}
	}
	return nil
}

// collectPruned appends to drop, in document order, the children of node
// that are objects meeting the conditions, evaluated on each object (then
// the context). The elements of an array are always checked; the members of
// an object only when the spec is recursive.
func (r *overrideRun) collectPruned(node gjson.Result, path string, spec pruneSpec, drop *[]pruned) error {
	if node.IsObject() && !spec.recursive {
		return nil
	}
	var err error
	eachChild(node, func(seg string, child gjson.Result) bool {
		p := joinPath(path, seg)
		hit := false
		if child.IsObject() {
			hit, err = r.matches([]byte(child.Raw), spec.conds, spec.all)
		}
		switch {
		case err != nil:
		case hit:
			*drop = append(*drop, pruned{path: p, member: node.IsObject()})
		default:
			err = r.collectPruned(child, p, spec, drop)
		}
		return err == nil
	})
	return err
}

// deletePruned removes a pruned object; a member removed with all its copies.
func (r *overrideRun) deletePruned(d pruned) error {
	for {
		size := len(r.body)
		var err error
		if r.body, err = sjson.DeleteBytes(r.body, d.path); err != nil {
			return err
		}
		if !d.member || len(r.body) == size || !gjson.GetBytes(r.body, d.path).Exists() {
			return nil
		}
	}
}

// ---- header operations

// header is the current value of a header: set by an earlier operation,
// else the client's, unless an operation removed it.
func (r *overrideRun) header(name string) (string, bool) {
	if v, ok := r.set[name]; ok {
		return v, true
	}
	if r.deleted[name] || r.in == nil {
		return "", false
	}
	v := headerValue(r.in.Headers, name)
	return v, v != ""
}

// setHeader sets a header; an empty value removes it. A value that cannot
// be sent (a control character, say from the body by sync_fields) fails the
// run rather than the upstream request.
func (r *overrideRun) setHeader(name, value string) error {
	if value == "" {
		r.deleteHeader(name)
		return nil
	}
	if !ValidHeaderValue(value) {
		return fmt.Errorf("header %s: the value contains a control character", name)
	}
	r.set[name] = value
	delete(r.deleted, name)
	r.ctx = nil
	return nil
}

func (r *overrideRun) deleteHeader(name string) {
	delete(r.set, name)
	r.deleted[name] = true
	r.ctx = nil
}

// setHeaderOp is set_header: keep_origin leaves a header an earlier
// operation set.
func (r *overrideRun) setHeaderOp(name string, keep bool, value func(current string) string) error {
	if keep && r.set[name] != "" {
		return nil
	}
	cur, _ := r.header(name)
	return r.setHeader(name, value(cur))
}

// copyHeader is copy_header / move_header; a missing source does nothing.
func (r *overrideRun) copyHeader(src, dst string, keep, move bool) error {
	v, ok := r.header(src)
	if !ok {
		return nil
	}
	if err := r.setHeaderOp(dst, keep, func(string) string { return v }); err != nil {
		return err
	}
	if move && src != dst {
		r.deleteHeader(src)
	}
	return nil
}

func (r *overrideRun) headerResult() HeaderOverride {
	out := HeaderOverride{Set: r.set, Delete: make([]string, 0, len(r.deleted))}
	for name := range r.deleted {
		out.Delete = append(out.Delete, name)
	}
	sort.Strings(out.Delete)
	return out
}
