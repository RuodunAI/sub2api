package standard

import (
	"errors"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The body is edited by path, in place of the bytes it names: everything
// else - key order, white space, unknown fields, escapes, numbers - is
// returned as the core handed it over. It is never decoded whole.
//
// The core hands over valid JSON without duplicate keys (it rejects other
// bodies with 400 before any plugin runs). An edited body is checked again
// all the same, because an edit of a body that is not JSON, or of a key that
// appears twice, could not say which bytes it changed: such a body is
// refused (INVALID_ARGUMENT, nothing is relayed) instead of being sent half
// edited.

// edit runs the type's FilterFields and the route's Edit on body.
func (t *Type) edit(body []byte, route *Route, r *Request) ([]byte, error) {
	if len(t.FilterFields) == 0 && route.Edit == nil {
		return body, nil
	}
	if !gjson.ValidBytes(body) {
		return nil, status.Error(codes.InvalidArgument, "the request body is not valid JSON")
	}
	var err error
	for _, f := range t.FilterFields {
		if body, err = DeleteJSON(body, f); err != nil {
			return nil, err
		}
	}
	if route.Edit == nil {
		return body, nil
	}
	return route.Edit(body, r)
}

// SetJSON sets the value at path (gjson/sjson dot syntax, plain keys) of
// the JSON document body, replacing the bytes of an existing value in
// place or adding the key at the end of its object. A key on path that
// appears twice in its object is an error.
func SetJSON(body []byte, path string, value any) ([]byte, error) {
	if err := uniquePath(body, path); err != nil {
		return nil, err
	}
	out, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set %s in the request body: %v", path, err)
	}
	return out, nil
}

// DeleteJSON removes path from the JSON document body, every occurrence of
// it, and returns body itself when path is absent.
func DeleteJSON(body []byte, path string) ([]byte, error) {
	for gjson.GetBytes(body, path).Exists() {
		out, err := sjson.DeleteBytes(body, path)
		if err == nil && len(out) == len(body) {
			err = errors.New("nothing was removed")
		}
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "delete %s from the request body: %v", path, err)
		}
		body = out
	}
	return body, nil
}

// uniquePath fails when a key of path appears more than once in the object
// that holds it. It reads body in place, without a copy.
func uniquePath(body []byte, path string) error {
	parent := gjson.Parse(unsafe.String(unsafe.SliceData(body), len(body)))
	for _, key := range strings.Split(path, ".") {
		var next gjson.Result
		n := 0
		parent.ForEach(func(k, v gjson.Result) bool {
			if k.Str == key {
				if n++; n == 1 {
					next = v
				}
			}
			return n < 2
		})
		switch n {
		case 0:
			return nil
		case 2:
			return status.Errorf(codes.InvalidArgument, "key %q appears twice in the request body", key)
		}
		parent = next
	}
	return nil
}
