package fieldcheck_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/fieldcheck"
)

var rules = fieldcheck.Fields{
	"host":     fieldcheck.String().MaxLen(5),
	"name":     fieldcheck.String().Required().Len(2, 4),
	"port":     fieldcheck.Integer().Range(1, 65535),
	"ratio":    fieldcheck.Number().Min(0).Max(1.5),
	"mode":     fieldcheck.String().OneOf("a", "b"),
	"key":      fieldcheck.String().Pattern(`^[!-~]+$`),
	"on":       fieldcheck.Bool(),
	"tags":     fieldcheck.Array().MaxLen(2),
	"hidden":   fieldcheck.Any(),
	"a/b":      fieldcheck.String(),
	"upstream": fieldcheck.Object(fieldcheck.Fields{"url": fieldcheck.String().Required()}),
}

// codes is field -> code of errs.
func codes(errs pluginsdk.FieldErrors) map[string]string {
	out := map[string]string{}
	for _, e := range errs {
		out[e.GetField()] = e.GetCode()
	}
	return out
}

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheckAccepts(t *testing.T) {
	ok := decode(t, `{"host":"h","name":"日本語","port":443,"ratio":1.5,"mode":"b","key":"k!","on":false,
		"tags":["x"],"hidden":{"any":[1]},"a/b":"","upstream":{"url":"u"}}`)
	if errs := fieldcheck.Check(rules, ok); len(errs) > 0 {
		t.Fatalf("errors = %v", errs)
	}
	if errs := fieldcheck.Check(rules, map[string]any{"name": "ab"}); len(errs) > 0 {
		t.Fatalf("optional fields may be absent: %v", errs)
	}
}

func TestCheckRefuses(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want map[string]string
	}{
		"required":         {`{}`, map[string]string{"name": "required"}},
		"required empty":   {`{"name":""}`, map[string]string{"name": "required"}},
		"min length":       {`{"name":"x"}`, map[string]string{"name": "minLength"}},
		"max length runes": {`{"name":"日本語です!"}`, map[string]string{"name": "maxLength"}},
		"string type":      {`{"name":"ab","host":5}`, map[string]string{"host": "type"}},
		"null":             {`{"name":"ab","host":null}`, map[string]string{"host": "type"}},
		"integer":          {`{"name":"ab","port":1.5}`, map[string]string{"port": "type"}},
		"minimum":          {`{"name":"ab","port":0}`, map[string]string{"port": "minimum"}},
		"maximum":          {`{"name":"ab","ratio":2}`, map[string]string{"ratio": "maximum"}},
		"enum":             {`{"name":"ab","mode":"c"}`, map[string]string{"mode": "enum"}},
		"pattern":          {`{"name":"ab","key":"a b"}`, map[string]string{"key": "pattern"}},
		"bool":             {`{"name":"ab","on":"true"}`, map[string]string{"on": "type"}},
		"max items":        {`{"name":"ab","tags":[1,2,3]}`, map[string]string{"tags": "maxItems"}},
		"unknown":          {`{"name":"ab","extra":1,"x~y":2}`, map[string]string{"extra": "unknown", "x~0y": "unknown"}},
		"escaped key":      {`{"name":"ab","a/b":1}`, map[string]string{"a~1b": "type"}},
		"nested":           {`{"name":"ab","upstream":{"token":"t"}}`, map[string]string{"upstream/url": "required", "upstream/token": "unknown"}},
		"nested type":      {`{"name":"ab","upstream":"u"}`, map[string]string{"upstream": "type"}},
		"one per field":    {`{"name":"日本語です!!!!!!!!"}`, map[string]string{"name": "maxLength"}},
	}
	for name, c := range cases {
		errs := fieldcheck.Check(rules, decode(t, c.raw))
		if got := codes(errs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
		if len(errs) != len(c.want) {
			t.Errorf("%s: %d errors, want %d", name, len(errs), len(c.want))
		}
	}
}

// Credentials and account settings: a field is read from the first object
// holding it, and an unknown key of either is refused.
func TestCheckObjects(t *testing.T) {
	creds := map[string]any{"name": "abc"}
	settings := map[string]any{"name": "x", "host": "h", "proxy": "p"}
	if got := codes(fieldcheck.Check(rules, creds, settings)); !reflect.DeepEqual(got, map[string]string{"proxy": "unknown"}) {
		t.Fatalf("errors = %v", got)
	}
}

// json.Number (accountform decodes with UseNumber) counts as a number.
func TestCheckJSONNumber(t *testing.T) {
	obj := map[string]any{"name": "ab", "port": json.Number("8080")}
	if errs := fieldcheck.Check(rules, obj); len(errs) > 0 {
		t.Fatalf("errors = %v", errs)
	}
	obj["port"] = json.Number("80.5")
	if got := codes(fieldcheck.Check(rules, obj)); got["port"] != "type" {
		t.Fatalf("errors = %v", got)
	}
}

func TestUnknown(t *testing.T) {
	errs := fieldcheck.Unknown([]string{"api_key"}, map[string]any{"api_key": "k", "b": 1}, map[string]any{"a": 1, "b": 2})
	if got := codes(errs); !reflect.DeepEqual(got, map[string]string{"a": "unknown", "b": "unknown"}) || errs[0].GetField() != "a" {
		t.Fatalf("errors = %v", errs)
	}
	if errs := fieldcheck.Unknown(nil); errs != nil {
		t.Fatalf("nothing to check = %v", errs)
	}
}
