package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A step-up answered for an opaque session switches the client to the new
// token of the session: the proof is spent with it and so are the requests
// after it.
func TestDoAdoptsTheRotatedToken(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/api/v1/me/verify":
			fmt.Fprint(w, `{"data":{"proof":"p1","access_token":"s2s_new"}}`)
		case r.Header.Get("X-Verify-Proof") == "" && r.URL.Path == "/api/v1/danger":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":"verification_required","details":{"scope":"x","context":"DELETE /api/v1/danger"}}}`)
		default:
			fmt.Fprint(w, `{"data":{}}`)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.Token, c.Password = "s2s_old", "pw"
	if r := c.Do(t, http.MethodDelete, "/api/v1/danger", nil); r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	c.Do(t, http.MethodGet, "/api/v1/me", nil)
	want := []string{
		"DELETE /api/v1/danger Bearer s2s_old",
		"POST /api/v1/me/verify Bearer s2s_old",
		"DELETE /api/v1/danger Bearer s2s_new",
		"GET /api/v1/me Bearer s2s_new",
	}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("requests\n%v\nwant\n%v", seen, want)
	}
}
