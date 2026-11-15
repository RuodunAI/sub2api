package store

import (
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseArray(t *testing.T) {
	type elem struct {
		s    string
		null bool
	}
	for _, c := range []struct {
		src  string
		want []elem
		err  bool
	}{
		{src: "{}", want: nil},
		{src: " { } ", want: nil},
		{src: "{a, b ,c}", want: []elem{{"a", false}, {"b", false}, {"c", false}}},
		{src: `{"",""}`, want: []elem{{"", false}, {"", false}}},
		{src: `{NULL,"NULL",Null}`, want: []elem{{"NULL", true}, {"NULL", false}, {"Null", true}}},
		{src: `{"a\"b","c\\d",e\,f}`, want: []elem{{`a"b`, false}, {`c\d`, false}, {"e,f", false}}},
		{src: `{\NULL}`, want: []elem{{"NULL", false}}},
		{src: `{"{x}","}"}`, want: []elem{{"{x}", false}, {"}", false}}},
		{src: "{{1,2},{3}}", want: []elem{{"1", false}, {"2", false}, {"3", false}}},
		{src: "[1:2]={1,2}", want: []elem{{"1", false}, {"2", false}}},
		{src: "{日本,\"語 x\"}", want: []elem{{"日本", false}, {"語 x", false}}},
		{src: "", err: true},
		{src: "{", err: true},
		{src: "{a", err: true},
		{src: `{"a}`, err: true},
		{src: "{a,}", err: true},
		{src: "{,a}", err: true},
		{src: "{a}x", err: true},
		{src: `{"a"b}`, err: true},
		{src: "[1:2]", err: true},
	} {
		var got []elem
		err := parseArray(c.src, func(s string, null bool) error {
			got = append(got, elem{s, null})
			return nil
		})
		if (err != nil) != c.err || (!c.err && !equalElems(got, c.want)) {
			t.Errorf("parseArray(%q) = %v, %v; want %v (error %v)", c.src, got, err, c.want, c.err)
		}
	}
}

func equalElems[E comparable](a, b []E) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The array parameters are the plain slices for pgx to encode (see Int64s),
// nil an empty one rather than NULL; Array is its slice as given.
func TestArrayValuesArePlainSlices(t *testing.T) {
	at := time.Date(2024, 1, 2, 3, 4, 5, 6, time.UTC)
	one := int64(1)
	for _, c := range []struct {
		v    driver.Valuer
		want any
	}{
		{Int64s{1, 2}, []int64{1, 2}},
		{Int64s(nil), []int64{}},
		{Strings{"a", ""}, []string{"a", ""}},
		{Strings(nil), []string{}},
		{Times{at}, []time.Time{at}},
		{Times(nil), []time.Time{}},
		{Bools{true}, []bool{true}},
		{Bools(nil), []bool{}},
		{NullStrings([]string{"x"}).(driver.Valuer), []string{"x"}},
		{Array[*int64]{&one, nil}, []*int64{&one, nil}},
	} {
		got, err := c.v.Value()
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%T(%v).Value() = %#v, %v; want %#v", c.v, c.v, got, err, c.want)
		}
	}
	if NullStrings(nil) != nil {
		t.Error("NullStrings(nil) is not NULL")
	}
}

func TestParseTimestamptz(t *testing.T) {
	utc := func(y int, mo time.Month, d, h, mi, s, ns int) time.Time {
		return time.Date(y, mo, d, h, mi, s, ns, time.UTC)
	}
	for _, c := range []struct {
		src  string
		want time.Time
	}{
		{"2024-01-02 03:04:05+00", utc(2024, 1, 2, 3, 4, 5, 0)},
		{"2024-01-02 11:04:05.5+08", utc(2024, 1, 2, 3, 4, 5, 500000000)},
		{"2024-01-02 03:04:05.123456-03:30", utc(2024, 1, 2, 6, 34, 5, 123456000)},
		{"1900-01-01 00:00:00+00:53:28", utc(1899, 12, 31, 23, 6, 32, 0)},
		{"12024-01-02 03:04:05+00", utc(12024, 1, 2, 3, 4, 5, 0)},
		{"0044-03-15 12:00:00+00 BC", utc(-43, 3, 15, 12, 0, 0, 0)},
		{"2024-02-29 00:00:00+00", utc(2024, 2, 29, 0, 0, 0, 0)},
	} {
		got, err := parseTimestamptz(c.src)
		if err != nil || !got.Equal(c.want) || got.Location() != time.Local {
			t.Errorf("parseTimestamptz(%q) = %v, %v; want %v in time.Local", c.src, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "infinity", "-infinity", "2024-01-02", "2024-01-02 03:04:05", "24-01-02 03:04:05+00",
		"2023-02-29 00:00:00+00", "2024-01-02 24:00:00+00", "2024-01-02 03:60:00+00", "2024-01-02 3:04:05+00",
		"2024-01-02 03:04:05+8", "2024-01-02 03:04:05.x+00", "2024-01-02T03:04:05+00"} {
		if got, err := parseTimestamptz(bad); err == nil {
			t.Errorf("parseTimestamptz(%q) = %v, want an error", bad, got)
		}
	}
	var ts Times
	if err := ts.Scan(`{"2024-01-02 03:04:05+00",infinity}`); err == nil || !strings.Contains(err.Error(), "infinity") {
		t.Errorf("Times.Scan of an infinite element = %v, want an error", err)
	}
}

func TestInfTime(t *testing.T) {
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, c := range []struct {
		src  any
		want InfTime
	}{
		{nil, InfTime{}},
		{at, InfTime{Time: at, Valid: true}},
		{"infinity", InfTime{Infinity: PositiveInfinity, Valid: true}},
		{[]byte("-infinity"), InfTime{Infinity: NegativeInfinity, Valid: true}},
	} {
		var got InfTime
		if err := got.Scan(c.src); err != nil || got != c.want {
			t.Errorf("Scan(%#v) = %+v, %v; want %+v", c.src, got, err, c.want)
		}
		v, err := c.want.Value()
		var again InfTime
		if err != nil || again.Scan(v) != nil || again != c.want {
			t.Errorf("%+v -> Value %#v -> %+v, %v", c.want, v, again, err)
		}
	}
	got := InfTime{Time: at, Valid: true}
	if err := got.Scan("2024-01-02 03:04:05+00"); err != nil || !got.Time.Equal(at) || got.Infinity != Finite {
		t.Errorf("Scan of text = %+v, %v", got, err)
	}
	if err := got.Scan(1); err == nil {
		t.Error("Scan(1): no error")
	}
}
