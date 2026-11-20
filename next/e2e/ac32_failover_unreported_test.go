package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
)

// AC 32: failover across accounts under forwarding v2. An upstream error of
// the first account's request, or a failure of its plugin before anything
// was sent (here: a Vertex service account that has no access token yet, so
// its Execute fails), moves the request to the next account; the request is
// billed once (DESIGN §4.3).
func TestAC32_FailoverAndUnreported(t *testing.T) {
	e := Setup(t)
	admin := e.Admin()
	m := e.Mock()

	t.Run("upstream error fails over, billed once", func(t *testing.T) {
		e := e.With(t)
		for _, stream := range []bool{false, true} {
			tn := e.NewTenant(admin, TenantOpts{Accounts: 2, Priorities: []int{50, 1}})
			first, second := tn.Accounts[0], tn.Accounts[1]
			m.SetRule(t, MockRule{APIKey: first.Key, Status: 500, Remaining: 1})
			defer m.ClearRule(t, first.Key)
			mark := m.Mark(t)
			g := e.Messages(tn.APIKey, MessagesBody(tn.Model, "failover", stream), nil)
			if g.Status != 200 {
				t.Fatalf("stream=%v: HTTP %d %s", stream, g.Status, g.Body)
			}
			if used := KeysUsed(m.Since(t, mark), "/v1/messages"); len(used) != 2 || used[0] != first.Key || used[1] != second.Key {
				t.Fatalf("stream=%v: upstream keys %v, want [first(500) second]", stream, used)
			}
			u := e.expectBilled(admin, tn.User.UserID, g.RequestID, second.ID, usageAnthropic, stream)
			if u.Get("attempts").Int() != 2 {
				t.Fatalf("stream=%v: attempts = %d, want 2: %s", stream, u.Get("attempts").Int(), u.Raw)
			}
		}
	})

	t.Run("plugin failure before sending fails over, account not penalized", func(t *testing.T) {
		e := e.With(t)
		k := fmt.Sprintf("AIza-fail-%s-%d", e.RunID, nextSeq())
		tn := e.singleAccount(admin, "grp-plugin-fail", AccountSpec{PluginKey: GeminiPlugin, Type: APIKeyType, APIKey: k, Priority: 1}, k, "gemini", "plugin-fail")
		geminiID := tn.Accounts[0].ID
		// Tried first (priority 50): a Vertex account whose Execute fails.
		e.EnsurePlugin(admin, "vertex", "")
		vertexID := tn.addAccount(e, admin, AccountSpec{PluginKey: "vertex", Type: "vertex_sa", Priority: 50,
			Credentials: map[string]any{"service_account_json": serviceAccountJSON(t, "e2e-"+e.RunID), "project_id": "e2e-project", "location": "global"}}, "").ID
		for i := 0; i < 2; i++ {
			mark := m.Mark(t)
			g := e.Gemini(tn.APIKey, tn.Model, "generateContent", "", GeminiBody("plugin failure"))
			if g.Status != 200 {
				t.Fatalf("request %d: HTTP %d %s", i, g.Status, g.Body)
			}
			for _, r := range m.Since(t, mark) {
				if strings.Contains(r.Path, "/projects/e2e-project/") {
					t.Fatalf("request %d: the failing Vertex account sent a request: %+v", i, r)
				}
			}
			oneMockCall(t, m, mark, k, "gemini fallback")
			u := e.expectBilled(admin, tn.User.UserID, g.RequestID, geminiID, usageGemini, false)
			// The Vertex account was tried first both times: a plugin failure
			// does not cool the account down.
			steps := u.Get("attempt_trail.steps").Array()
			if len(steps) < 2 || steps[0].Get("account_id").Int() != vertexID {
				t.Fatalf("request %d: attempt trail does not start with the Vertex account %d: %s", i, vertexID, u.Get("attempt_trail").Raw)
			}
		}
	})
}

// serviceAccountJSON is a Google service account key file with a fresh RSA
// key (nothing accepts it: the token endpoint is the mock, which has none).
func serviceAccountJSON(t testing.TB, name string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "e2e-project",
		"private_key_id": fmt.Sprintf("%x", big.NewInt(0).SetBytes(der[:8])),
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   name + "@e2e-project.iam.gserviceaccount.com",
		"client_id":      "1234567890",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	return string(raw)
}
