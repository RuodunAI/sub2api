package manifest

import "testing"

func TestCoreRouteOf(t *testing.T) {
	for p, want := range map[string]string{
		"/api": "/api", "/api/": "/api", "/API": "/api",
		"/api/v1": "/api/v1", "/api/v1/accounts": "/api/v1", "/Api/V1/x": "/api/v1",
		"/api/:version/x": "/api/v1", "/api/*rest": "/api/v1",
		"/plugin-ui/x/y": "/plugin-ui", "/healthz": "/healthz", "/HEALTHZ/x": "/healthz",
	} {
		if got, ok := CoreRouteOf(p); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", p, got, ok, want)
		}
	}
	for _, p := range []string{"/apifoo", "/api/v3/chat/completions", "/api/v1x", "/api/v2/x",
		"/doubao/api/v3/x", "/v1/messages", "/healthcheck", "/plugin-uix", "/"} {
		if got, ok := CoreRouteOf(p); ok {
			t.Errorf("%s reserved as %s", p, got)
		}
	}
}

func TestConsoleRouteOf(t *testing.T) {
	for p, want := range map[string]string{
		"/account-usage": "/account-usage", "/account-usage/overview": "/account-usage",
		"/Account-Usage/:id": "/account-usage", "/segments/x": "/segments", "/assets/*rest": "/assets",
	} {
		if got, ok := ConsoleRouteOf("GET", p); !ok || got != want {
			t.Errorf("GET %s: %q %v, want %q", p, got, ok, want)
		}
	}
	for _, c := range [][2]string{{"POST", "/account-usage"}, {"GET", "/account-usagex"}, {"HEAD", "/v1/account-usage"}} {
		if got, ok := ConsoleRouteOf(c[0], c[1]); ok {
			t.Errorf("%s %s reserved as %s", c[0], c[1], got)
		}
	}
}
