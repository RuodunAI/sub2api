package store

import (
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTimesScan(t *testing.T) {
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		src  any
		want Times
		err  string
	}{
		{nil, nil, ""},
		{[]time.Time{at}, Times{at}, ""},
		{[]any{at}, Times{at}, ""},
		{`{"2024-01-02 03:04:05+00"}`, Times{at}, ""},
		{"{}", Times{}, ""},
		{[]any{at, "x"}, nil, "element 1"},
		{int64(1), nil, "cannot scan int64"},
	}
	for _, c := range cases {
		var got Times
		err := got.Scan(c.src)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("Scan(%#v) error = %v, want %q", c.src, err, c.err)
			}
			continue
		}
		if err != nil || len(got) != len(c.want) || (c.want == nil) != (got == nil) {
			t.Errorf("Scan(%#v) = %#v, %v; want %#v", c.src, got, err, c.want)
			continue
		}
		for i := range got {
			if !got[i].Equal(c.want[i]) {
				t.Errorf("Scan(%#v)[%d] = %v, want %v", c.src, i, got[i], c.want[i])
			}
		}
	}
	if v, _ := Times(nil).Value(); !reflect.DeepEqual(v, []time.Time{}) {
		t.Errorf("Times(nil).Value() = %#v, want the empty array", v)
	}
}

func TestJSONScanValue(t *testing.T) {
	j := JSON[[]string]{V: []string{"stale"}}
	for _, src := range []any{nil, []byte(`null`), "null"} {
		if err := j.Scan(src); err != nil || j.V != nil {
			t.Errorf("Scan(%#v) = %#v, %v; want the zero value", src, j.V, err)
		}
	}
	if err := j.Scan([]byte(`["a"]`)); err != nil || !reflect.DeepEqual(j.V, []string{"a"}) {
		t.Errorf("Scan = %#v, %v", j.V, err)
	}
	if err := j.Scan(`{"a":1}`); err == nil {
		t.Error("Scan of an object into []string: no error")
	}
	if err := j.Scan(1); err == nil || !strings.Contains(err.Error(), "cannot scan int") {
		t.Errorf("Scan(1) error = %v", err)
	}
	v, err := JSON[map[string]int]{V: map[string]int{"n": 1}}.Value()
	if err != nil || v != `{"n":1}` {
		t.Errorf("Value() = %#v, %v", v, err)
	}
	if _, err := (JSON[func()]{}).Value(); err == nil {
		t.Error("Value of a func: no error")
	}
}

func TestBytesValueScan(t *testing.T) {
	for _, c := range []struct {
		in   Bytes
		want driver.Value
	}{{nil, nil}, {Bytes{}, []byte{}}, {Bytes("ab"), []byte("ab")}} {
		v, err := c.in.Value()
		if err != nil || !reflect.DeepEqual(v, c.want) {
			t.Errorf("Bytes(%#v).Value() = %#v, %v; want %#v", c.in, v, err, c.want)
		}
	}
	src := []byte("ab")
	var b Bytes
	if err := b.Scan(src); err != nil || string(b) != "ab" {
		t.Fatalf("Scan = %#v, %v", b, err)
	}
	src[0] = 'x'
	if string(b) != "ab" {
		t.Error("Scan kept the source buffer")
	}
	if err := b.Scan("cd"); err != nil || string(b) != "cd" {
		t.Errorf("Scan(string) = %#v, %v", b, err)
	}
	if err := b.Scan(nil); err != nil || b != nil {
		t.Errorf("Scan(nil) = %#v, %v; want nil", b, err)
	}
	if err := b.Scan(1); err == nil {
		t.Error("Scan(1): no error")
	}
}

func TestInt64sScan(t *testing.T) {
	cases := []struct {
		src  any
		want Int64s
		err  string
	}{
		{nil, nil, ""},
		{[]int64{1, 2}, Int64s{1, 2}, ""},
		{[]int64{}, Int64s{}, ""},
		{[]any{int64(1), int32(2), int16(3)}, Int64s{1, 2, 3}, ""},
		{"{4,-5}", Int64s{4, -5}, ""},
		{[]byte("{6}"), Int64s{6}, ""},
		{"{}", Int64s{}, ""},
		{[]any{int64(1), nil}, nil, "element 1"},
		{"{1,NULL}", nil, "NULL"},
		{"{a}", nil, "invalid"},
		{int64(1), nil, "cannot scan int64"},
	}
	for _, c := range cases {
		var got Int64s
		err := got.Scan(c.src)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("Scan(%#v) error = %v, want %q", c.src, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Scan(%#v) = %#v, %v; want %#v", c.src, got, err, c.want)
		}
	}
}

func TestStringsScan(t *testing.T) {
	cases := []struct {
		src  any
		want Strings
		err  string
	}{
		{nil, nil, ""},
		{[]string{"a"}, Strings{"a"}, ""},
		{[]any{"a", "b"}, Strings{"a", "b"}, ""},
		{`{a,"b,c","d\"e",""}`, Strings{"a", "b,c", `d"e`, ""}, ""},
		{[]byte(`{x}`), Strings{"x"}, ""},
		{"{}", Strings{}, ""},
		{[]any{"a", 1}, nil, "element 1"},
		{"{a,NULL}", nil, "NULL"},
		{true, nil, "cannot scan bool"},
	}
	for _, c := range cases {
		var got Strings
		err := got.Scan(c.src)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("Scan(%#v) error = %v, want %q", c.src, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Scan(%#v) = %#v, %v; want %#v", c.src, got, err, c.want)
		}
	}
}
