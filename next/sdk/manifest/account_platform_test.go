package manifest

import "testing"

func TestServesPluginFacts(t *testing.T) {
	p := Platform{ID: "video", Endpoints: []Endpoint{
		{ID: "plain"},
		{ID: "usage", UsageSource: UsageSourcePlugin},
		{ID: "submit", Task: &AsyncTaskEndpoint{Action: TaskActionSubmit}},
		{ID: "query", Task: &AsyncTaskEndpoint{Action: TaskActionQuery}},
	}}
	for _, tc := range []struct {
		endpoints []string
		want      bool
	}{
		{nil, true},
		{[]string{"plain", "query"}, false},
		{[]string{"plain", "usage"}, true},
		{[]string{"submit"}, true},
	} {
		if got := (AccountPlatform{Platform: "video", Endpoints: tc.endpoints}).ServesPluginFacts(p); got != tc.want {
			t.Errorf("endpoints %v: ServesPluginFacts = %v, want %v", tc.endpoints, got, tc.want)
		}
	}
}
