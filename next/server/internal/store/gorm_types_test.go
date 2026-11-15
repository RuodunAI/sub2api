package store

import (
	"reflect"
	"strings"
	"testing"
)

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
