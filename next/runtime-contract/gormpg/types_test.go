package gormpg

import (
	"fmt"
	"reflect"
	"testing"
)

func TestArray(t *testing.T) {
	if v, err := Array[int64](nil).Value(); v != nil || err != nil {
		t.Fatalf("nil Array = %v, %v; want NULL", v, err)
	}
	if v, _ := (Array[int64]{}).Value(); !reflect.DeepEqual(v, []int64{}) {
		t.Fatalf("empty Array = %#v; want the empty array", v)
	}
	var a Array[string]
	if err := a.Scan(`{a,"b c"}`); err != nil || !reflect.DeepEqual(a, Array[string]{"a", "b c"}) {
		t.Fatalf("Scan = %q, %v", a, err)
	}
	if err := a.Scan(nil); err != nil || a != nil {
		t.Fatalf("Scan(NULL) = %q, %v", a, err)
	}
	type doc []byte
	one := 1
	for _, c := range []struct {
		a    fmt.Stringer
		want string
	}{
		{Array[int64]{1, 2}, "[1 2]"},
		{Array[*int]{&one, nil}, "[1 NULL]"},
		{Array[doc]{doc(`{"k":1}`), nil}, `[{"k":1} NULL]`},
		{Array[any]{"x", nil, doc("d")}, "[x NULL d]"},
	} {
		if got := c.a.String(); got != c.want {
			t.Errorf("String = %s, want %s", got, c.want)
		}
	}
}
