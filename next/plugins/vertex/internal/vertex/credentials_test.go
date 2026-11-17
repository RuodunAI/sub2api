package vertex

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"maps"
	"slices"
	"strings"
	"testing"
)

// ecPEM is a PKCS #8 key that is not RSA.
func ecPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestValidateRejects(t *testing.T) {
	const marker = "SECRET-MARKER-DO-NOT-ECHO"
	for _, tc := range []struct {
		name     string
		typ      string
		creds    map[string]any
		settings map[string]any
		want     string
	}{
		{"unknown type", "vertex_other", map[string]any{}, nil, "account_type:unsupported"},

		{"sa: no file", TypeServiceAccount, map[string]any{}, nil, "service_account_json:required"},
		{"sa: not JSON", TypeServiceAccount, map[string]any{"service_account_json": "{" + marker}, nil, "service_account_json:invalid"},
		{"sa: JSON array", TypeServiceAccount, map[string]any{"service_account_json": `["` + marker + `"]`}, nil, "service_account_json:invalid"},
		{"sa: a user file", TypeServiceAccount, map[string]any{"service_account_json": userJSON(nil)}, nil, "service_account_json:invalid"},
		{"sa: no type", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"type": nil})}, nil, "service_account_json:invalid"},
		{"sa: no client_email", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"client_email": " "})}, nil, "service_account_json:invalid"},
		{"sa: no private_key", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"private_key": nil})}, nil, "service_account_json:invalid"},
		{"sa: private_key not PEM", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"private_key": marker})}, nil, "service_account_json:invalid"},
		{"sa: private_key PEM garbage", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{
			"private_key": "-----BEGIN PRIVATE KEY-----\nU0VDUkVULU1BUktFUg==\n-----END PRIVATE KEY-----\n"})}, nil, "service_account_json:invalid"},
		{"sa: private_key not RSA", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"private_key": ecPEM(t)})}, nil, "service_account_json:invalid"},
		{"sa: foreign token_uri", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"token_uri": "https://evil.example/token"})}, nil, "service_account_json:invalid"},
		{"sa: no token_uri", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"token_uri": nil})}, nil, "service_account_json:invalid"},
		{"sa: no project anywhere", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"project_id": nil})}, nil, "project_id:required"},
		{"sa: bad project", TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil)}, map[string]any{fieldProject: "Bad_Project"}, "project_id:pattern"},
		{"sa: bad project in the file", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"project_id": "x/y"})}, nil, "project_id:pattern"},
		{"sa: location is a host", TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil)}, map[string]any{fieldLocation: "evil.example/x"}, "location:pattern"},

		{"user: no file", TypeAuthorizedUser, map[string]any{}, nil, "authorized_user_json:required"},
		{"user: not JSON", TypeAuthorizedUser, map[string]any{"authorized_user_json": marker}, nil, "authorized_user_json:invalid"},
		{"user: a service account file", TypeAuthorizedUser, map[string]any{"authorized_user_json": saJSON(nil)}, nil, "authorized_user_json:invalid"},
		{"user: no client_id", TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(map[string]any{"client_id": nil})}, nil, "authorized_user_json:invalid"},
		{"user: no client_secret", TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(map[string]any{"client_secret": ""})}, nil, "authorized_user_json:invalid"},
		{"user: no refresh_token", TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(map[string]any{"refresh_token": nil})}, nil, "authorized_user_json:invalid"},
		{"user: no project anywhere", TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(map[string]any{"quota_project_id": nil})}, nil, "project_id:required"},

		{"apikey: none", TypeAPIKey, map[string]any{}, nil, "api_key:required"},
		{"apikey: too short", TypeAPIKey, map[string]any{fieldAPIKey: "short"}, nil, "api_key:pattern"},
		{"apikey: header injection", TypeAPIKey, map[string]any{fieldAPIKey: "AQ.key-value\r\nx-evil: 1"}, nil, "api_key:pattern"},
		{"apikey: project only", TypeAPIKey, map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}, map[string]any{fieldProject: testProject}, "location:required"},
		{"apikey: location only", TypeAPIKey, map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}, map[string]any{fieldLocation: "global"}, "project_id:required"},
		{"apikey: bad location", TypeAPIKey, map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}, map[string]any{fieldProject: testProject, fieldLocation: "US"}, "location:pattern"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _, _ := validate(t, tc.typ, tc.creds, tc.settings)
			if got := fieldErrors(resp); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("errors = %v, want [%s]", got, tc.want)
			}
			for _, e := range resp.GetErrors() {
				if strings.Contains(e.GetMessage(), marker) || strings.Contains(e.GetMessage(), "BEGIN") {
					t.Fatalf("the message echoes the submitted secret: %s", e.GetMessage())
				}
			}
			if resp.GetNormalizedCredentialsJson() != "" || resp.GetNormalizedSettingsJson() != "" {
				t.Fatalf("a refusal must not normalize: %+v", resp)
			}
		})
	}
}

func TestValidateRejectsNonObjects(t *testing.T) {
	p := newTestPlugin(Endpoints{})
	for _, tc := range []struct{ creds, settings string }{
		{`[1]`, `{}`}, {`{`, `{}`}, {`{}`, `"x"`},
	} {
		resp, err := p.ValidateCredentials(t.Context(), validateRequest(TypeAPIKey, tc.creds, tc.settings))
		if err != nil || !slices.Equal(fieldErrors(resp), []string{":invalid"}) {
			t.Errorf("%s / %s: %v %v", tc.creds, tc.settings, fieldErrors(resp), err)
		}
	}
}

// The project comes from the key file unless the account names one, and the
// location defaults to global.
func TestValidateFillsTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typ      string
		creds    map[string]any
		settings map[string]any
		want     target
	}{
		{"sa: from the file", TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil)}, nil, target{testProject, LocationGlobal}},
		{"sa: empty strings", TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil)},
			map[string]any{fieldProject: "", fieldLocation: ""}, target{testProject, LocationGlobal}},
		{"sa: the account's own", TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil)},
			map[string]any{fieldProject: "other-project-1", fieldLocation: "europe-west4"}, target{"other-project-1", "europe-west4"}},
		{"sa: file without project", TypeServiceAccount, map[string]any{"service_account_json": saJSON(map[string]any{"project_id": nil})},
			map[string]any{fieldProject: "other-project-1"}, target{"other-project-1", LocationGlobal}},
		{"user: quota project", TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(nil)}, nil, target{testProject, LocationGlobal}},
		{"user: the account's own", TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(map[string]any{"quota_project_id": nil})},
			map[string]any{fieldProject: "other-project-1", fieldLocation: "us-east5"}, target{"other-project-1", "us-east5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _, settings := validate(t, tc.typ, tc.creds, tc.settings)
			if len(resp.GetErrors()) > 0 {
				t.Fatalf("errors: %v", fieldErrors(resp))
			}
			if got := (target{Project: str(settings[fieldProject]), Location: str(settings[fieldLocation])}); got != tc.want {
				t.Fatalf("target = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func str(v any) string { s, _ := v.(string); return s }

// A new account, or one whose key changed, loses any stored token and gets
// the expired expires_at that makes the core fetch the first token; an
// account whose token belongs to the submitted key is left exactly as it is.
func TestValidateBootstrapsToken(t *testing.T) {
	sa := saJSON(nil)
	saKey, err := parseServiceAccount(sa)
	if err != nil {
		t.Fatal(err)
	}
	user := userJSON(nil)
	userKey, err := parseAuthorizedUser(user)
	if err != nil {
		t.Fatal(err)
	}
	live := testNow.Unix() + 1800

	for _, tc := range []struct {
		name  string
		typ   string
		creds map[string]any
		kept  bool
	}{
		{"sa: new account", TypeServiceAccount, map[string]any{"service_account_json": sa}, false},
		{"sa: token of this key", TypeServiceAccount,
			map[string]any{"service_account_json": sa, fieldAccessToken: "ya29.live", fieldExpiresAt: live, fieldTokenFor: saKey.fingerprint()}, true},
		{"sa: expiry stored as a string", TypeServiceAccount,
			map[string]any{"service_account_json": sa, fieldAccessToken: "ya29.live", fieldExpiresAt: "1800001800", fieldTokenFor: saKey.fingerprint()}, true},
		{"sa: key rotated", TypeServiceAccount,
			map[string]any{"service_account_json": saJSON(map[string]any{"private_key_id": "ffff"}), fieldAccessToken: "ya29.old", fieldExpiresAt: live, fieldTokenFor: saKey.fingerprint()}, false},
		{"sa: another service account", TypeServiceAccount,
			map[string]any{"service_account_json": saJSON(map[string]any{"client_email": "other@x.iam.gserviceaccount.com"}), fieldAccessToken: "ya29.old", fieldExpiresAt: live, fieldTokenFor: saKey.fingerprint()}, false},
		{"sa: token without fingerprint", TypeServiceAccount,
			map[string]any{"service_account_json": sa, fieldAccessToken: "ya29.pasted", fieldExpiresAt: live}, false},
		{"sa: token without expiry", TypeServiceAccount,
			map[string]any{"service_account_json": sa, fieldAccessToken: "ya29.live", fieldTokenFor: saKey.fingerprint()}, false},
		{"sa: fingerprint without token", TypeServiceAccount,
			map[string]any{"service_account_json": sa, fieldExpiresAt: live, fieldTokenFor: saKey.fingerprint()}, false},
		{"user: new account", TypeAuthorizedUser, map[string]any{"authorized_user_json": user}, false},
		{"user: token of this key", TypeAuthorizedUser,
			map[string]any{"authorized_user_json": user, fieldAccessToken: "ya29.live", fieldExpiresAt: live, fieldTokenFor: userKey.fingerprint()}, true},
		{"user: refresh token replaced", TypeAuthorizedUser,
			map[string]any{"authorized_user_json": userJSON(map[string]any{"refresh_token": "1//0another"}), fieldAccessToken: "ya29.old", fieldExpiresAt: live, fieldTokenFor: userKey.fingerprint()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, creds, _ := validate(t, tc.typ, tc.creds, fullTarget)
			if len(resp.GetErrors()) > 0 {
				t.Fatalf("errors: %v", fieldErrors(resp))
			}
			if tc.kept {
				if resp.GetNormalizedCredentialsJson() != "" {
					t.Fatalf("credentials with a token of the submitted key were rewritten: %s", resp.GetNormalizedCredentialsJson())
				}
				return
			}
			_, hasToken := creds[fieldAccessToken]
			_, hasFor := creds[fieldTokenFor]
			if hasToken || hasFor || creds[fieldExpiresAt] != float64(expiredAt) {
				t.Fatalf("stale token not reset: token %v, token_for %v, expires_at %v", hasToken, hasFor, creds[fieldExpiresAt])
			}
			// The key file itself is stored as submitted.
			field := keyKinds[tc.typ].field
			if creds[field] != tc.creds[field] {
				t.Fatal("the key file was rewritten")
			}
			// The core reads the bootstrap value as an expiry in the past.
			if exp, ok := expiry.ExpiresAt(creds); !ok || !exp.Before(testNow) {
				t.Fatalf("expires_at %v is not a readable, past expiry", creds[fieldExpiresAt])
			}
			// Saving the result again changes nothing.
			again, _, _ := validate(t, tc.typ, creds, fullTarget)
			if len(again.GetErrors()) > 0 || again.GetNormalizedCredentialsJson() != "" || again.GetNormalizedSettingsJson() != "" {
				t.Fatalf("validation is not idempotent: %+v", again)
			}
		})
	}
}

// The fingerprint follows the identity of the key, not the file's layout.
func TestFingerprint(t *testing.T) {
	a, _ := parseServiceAccount(saJSON(nil))
	b, _ := parseServiceAccount(saJSON(map[string]any{"project_id": "another-project-9", "client_id": "999"}))
	c, _ := parseServiceAccount(saJSON(map[string]any{"private_key_id": "ffff"}))
	d, _ := parseServiceAccount(saJSON(map[string]any{"client_email": "other@x.iam.gserviceaccount.com"}))
	if a.fingerprint() != b.fingerprint() || a.fingerprint() == c.fingerprint() || a.fingerprint() == d.fingerprint() {
		t.Fatal("service account fingerprint must follow client_email and private_key_id only")
	}
	if len(a.fingerprint()) != 64 || strings.Contains(a.fingerprint(), testEmail) {
		t.Fatalf("fingerprint %q is not a SHA-256", a.fingerprint())
	}
	u, _ := parseAuthorizedUser(userJSON(nil))
	v, _ := parseAuthorizedUser(userJSON(map[string]any{"quota_project_id": "another-project-9"}))
	w, _ := parseAuthorizedUser(userJSON(map[string]any{"refresh_token": "1//0another"}))
	x, _ := parseAuthorizedUser(userJSON(map[string]any{"client_id": "another.apps.googleusercontent.com"}))
	if u.fingerprint() != v.fingerprint() || u.fingerprint() == w.fingerprint() || u.fingerprint() == x.fingerprint() {
		t.Fatal("user fingerprint must follow client_id and refresh_token only")
	}
}

func TestValidateAPIKey(t *testing.T) {
	key := map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}
	for name, settings := range map[string]map[string]any{
		"express":       nil,
		"empty strings": {fieldProject: "", fieldLocation: ""},
		"full":          {fieldProject: testProject, fieldLocation: "us-central1"},
		"global":        {fieldProject: testProject, fieldLocation: "global"},
	} {
		resp, creds, saved := validate(t, TypeAPIKey, key, settings)
		if len(resp.GetErrors()) > 0 || !maps.Equal(creds, key) || !maps.Equal(saved, settings) {
			t.Errorf("%s: %+v", name, resp)
		}
	}
	// The key is checked and trimmed by apikey.Spec, as in every API key form.
	resp, creds, _ := validate(t, TypeAPIKey, map[string]any{fieldAPIKey: "  AQ.Ab8RN6-example\n"}, nil)
	if len(resp.GetErrors()) > 0 || creds[fieldAPIKey] != "AQ.Ab8RN6-example" {
		t.Errorf("trimmed key: %+v %v", resp, creds)
	}
	resp, _, _ = validate(t, TypeAPIKey, map[string]any{fieldAPIKey: 12345678}, nil)
	if got := fieldErrors(resp); !slices.Equal(got, []string{"api_key:type"}) {
		t.Errorf("key that is not a string: %v", got)
	}
}
