package vertex

import (
	"strings"
	"testing"
)

// TestFormSchemaConstraintsStillRefused keeps every constraint of the JSON
// Schema forms the native account forms replaced (forms/{sa,authorized_user,
// apikey}.schema.json, deleted with UI 4.0): for each one, an input that
// schema refused, written out here, is still refused by ValidateCredentials
// at the same field. The core no longer checks a schema, so this is the
// save's only check besides the SDK's refusal of unknown keys.
func TestFormSchemaConstraintsStillRefused(t *testing.T) {
	type input struct {
		name            string
		creds, settings map[string]any
		field           string
	}
	// The constraints of a key file type: the file, then project and location.
	keyFile := func(field, file string) []input {
		return []input{
			{field + " required", map[string]any{}, nil, field},
			{field + " type string", map[string]any{field: 12}, nil, field},
			{field + " minLength 2", map[string]any{field: "{"}, nil, field},
			{field + " maxLength 32768", map[string]any{field: strings.Repeat("x", 32769)}, nil, field},
			{"project_id pattern", map[string]any{field: file}, map[string]any{fieldProject: "Bad_Project"}, fieldProject},
			{"project_id pattern (too short)", map[string]any{field: file}, map[string]any{fieldProject: "abc"}, fieldProject},
			{"location pattern", map[string]any{field: file}, map[string]any{fieldLocation: "US"}, fieldLocation},
			{"location pattern (a host)", map[string]any{field: file}, map[string]any{fieldLocation: "evil.example/x"}, fieldLocation},
		}
	}
	const key = "AQ.Ab8RN6-example"
	cases := map[string][]input{
		TypeServiceAccount: keyFile("service_account_json", saJSON(nil)),
		TypeAuthorizedUser: keyFile("authorized_user_json", userJSON(nil)),
		TypeAPIKey: {
			{"api_key required", map[string]any{}, nil, fieldAPIKey},
			{"api_key type string", map[string]any{fieldAPIKey: 123456789}, nil, fieldAPIKey},
			{"api_key minLength 8", map[string]any{fieldAPIKey: "AQ.1234"}, nil, fieldAPIKey},
			{"api_key maxLength 512", map[string]any{fieldAPIKey: strings.Repeat("k", 513)}, nil, fieldAPIKey},
			{"api_key pattern (space)", map[string]any{fieldAPIKey: "AQ.Ab8 RN6"}, nil, fieldAPIKey},
			{"project_id pattern", map[string]any{fieldAPIKey: key}, map[string]any{fieldProject: "Bad_Project", fieldLocation: "us-central1"}, fieldProject},
			{"location pattern", map[string]any{fieldAPIKey: key}, map[string]any{fieldProject: testProject, fieldLocation: "US"}, fieldLocation},
		},
	}
	for typ, inputs := range cases {
		for _, in := range inputs {
			t.Run(typ+"/"+in.name, func(t *testing.T) {
				resp, _, _ := validate(t, typ, in.creds, in.settings)
				got := fieldErrors(resp)
				if len(got) != 1 || !strings.HasPrefix(got[0], in.field+":") {
					t.Fatalf("errors = %v, want one at %s", got, in.field)
				}
			})
		}
	}
}
