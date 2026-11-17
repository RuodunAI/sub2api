package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

// Credential and settings fields besides the key file (keyKinds).
const (
	fieldAPIKey      = "api_key"
	fieldAccessToken = "access_token"
	fieldExpiresAt   = "expires_at"
	// fieldTokenFor holds the fingerprint of the key the stored access token
	// was issued for.
	fieldTokenFor = "token_for"
	fieldProject  = "project_id"
	fieldLocation = "location"
)

// expiredAt is the expires_at of an account that has no access token yet: a
// readable expiry in the past, so the core's next refresh pass fetches the
// first token (it skips accounts without a readable expiry, CONTRACTS §48.3).
const expiredAt = 1

var (
	projectPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	locationPattern = regexp.MustCompile(`^[a-z0-9-]{2,32}$`)
	apiKeyPattern   = regexp.MustCompile(`^[!-~]{8,512}$`)
)

// expiry reads expires_at the way the core does.
var expiry = &manifest.AccountRefresh{}

// object is a decoded JSON object (numbers kept as written).
type object map[string]any

// decodeObject decodes a JSON object; blank input is an empty one.
func decodeObject(raw string) (object, error) {
	o := object{}
	if strings.TrimSpace(raw) == "" {
		return o, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&o); err != nil {
		return nil, err
	}
	if o == nil {
		o = object{}
	}
	return o, nil
}

// str is the trimmed string value of k, "" when absent or not a string.
func (o object) str(k string) string {
	s, _ := o[k].(string)
	return strings.TrimSpace(s)
}

func (o object) json() []byte {
	b, _ := json.Marshal(o)
	return b
}

// target is where an account's requests go.
type target struct {
	Project  string
	Location string
}

func readTarget(settings object) target {
	return target{Project: settings.str(fieldProject), Location: settings.str(fieldLocation)}
}

// express reports the address without project and location, which only an
// API key can use.
func (t target) express() bool { return t.Project == "" && t.Location == "" }

// check refuses a target requests cannot be built for. expressOK allows
// leaving both parts empty.
func (t target) check(expressOK bool) *pluginv1.FieldError {
	missing := "is required / 必填"
	if expressOK {
		missing = "is required when the other of project ID and location is set: fill in both or neither / 项目 ID 和区域要么都填，要么都不填"
	}
	switch {
	case expressOK && t.express():
		return nil
	case t.Project == "":
		return fieldError(fieldProject, "required", "project ID "+missing)
	case t.Location == "":
		return fieldError(fieldLocation, "required", "location "+missing)
	case !projectPattern.MatchString(t.Project):
		return fieldError(fieldProject, "pattern", "not a Google Cloud project ID / 不是合法的 Google Cloud 项目 ID")
	case !locationPattern.MatchString(t.Location):
		return fieldError(fieldLocation, "pattern", "not a Vertex AI location / 不是合法的 Vertex AI 区域")
	}
	return nil
}

func fieldError(field, code, msg string) *pluginv1.FieldError {
	return &pluginv1.FieldError{Field: field, Code: code, Message: msg}
}

// ---------------------------------------------------------------- validation

// ValidateCredentials implements pluginsdk.Platform. Besides checking the
// submitted fields it normalizes them (the core stores the normalized form):
//
//   - a key file account without project ID takes the one its file names,
//     and without location takes "global";
//   - a key file account whose stored access token is missing, was issued
//     for another key or has no readable expiry loses the token and gets
//     expires_at 1, which makes the core fetch a token on its next refresh
//     pass (bootstrapToken).
func (p *Plugin) ValidateCredentials(_ context.Context, in *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
	creds, err := decodeObject(in.GetCredentialsJson())
	if err != nil {
		return invalid(fieldError("", "invalid", "credentials must be a JSON object / 凭证必须是 JSON 对象")), nil
	}
	settings, err := decodeObject(in.GetSettingsJson())
	if err != nil {
		return invalid(fieldError("", "invalid", "settings must be a JSON object / 设置必须是 JSON 对象")), nil
	}
	credsBefore, settingsBefore := creds.json(), settings.json()

	var bad *pluginv1.FieldError
	typ := in.GetAccountType()
	switch kind, keyed := keyKinds[typ]; {
	case keyed:
		bad = validateKeyAccount(kind, creds, settings)
	case typ == TypeAPIKey:
		bad = validateAPIKeyAccount(creds, settings)
	default:
		bad = fieldError("account_type", "unsupported", "unsupported account type "+strconv.Quote(typ)+" / 不支持的账号类型")
	}
	if bad != nil {
		return invalid(bad), nil
	}
	return &pluginv1.ValidateCredentialsResponse{
		NormalizedCredentialsJson: changed(credsBefore, creds),
		NormalizedSettingsJson:    changed(settingsBefore, settings),
	}, nil
}

func invalid(e *pluginv1.FieldError) *pluginv1.ValidateCredentialsResponse {
	return &pluginv1.ValidateCredentialsResponse{Errors: []*pluginv1.FieldError{e}}
}

// changed is o's JSON when it differs from before, else "" (keep the input).
func changed(before []byte, o object) string {
	after := o.json()
	if bytes.Equal(before, after) {
		return ""
	}
	return string(after)
}

// validateKeyAccount checks a key file account and fills in what the file
// implies: the project, the default location and the token state.
func validateKeyAccount(kind keyKind, creds, settings object) *pluginv1.FieldError {
	raw := creds.str(kind.field)
	if raw == "" {
		return fieldError(kind.field, "required", "paste the JSON key file / 请粘贴 JSON 密钥文件的内容")
	}
	key, err := kind.parse(raw)
	if err != nil {
		return fieldError(kind.field, "invalid", err.Error())
	}
	if settings.str(fieldProject) == "" && key.project() != "" {
		settings[fieldProject] = key.project()
	}
	if settings.str(fieldLocation) == "" {
		settings[fieldLocation] = LocationGlobal
	}
	if bad := readTarget(settings).check(false); bad != nil {
		return bad
	}
	bootstrapToken(creds, key.fingerprint())
	return nil
}

// bootstrapToken keeps a stored access token only when it was issued for the
// key with this fingerprint and has a readable expiry. Otherwise the token is
// dropped and expires_at becomes expiredAt, so the next refresh pass (or the
// refresh right after the save) fetches one for the submitted key.
func bootstrapToken(creds object, keyFingerprint string) {
	_, readable := expiry.ExpiresAt(creds)
	if readable && creds.str(fieldAccessToken) != "" && creds.str(fieldTokenFor) == keyFingerprint {
		return
	}
	delete(creds, fieldAccessToken)
	delete(creds, fieldTokenFor)
	creds[fieldExpiresAt] = expiredAt
}

// validateAPIKeyAccount checks an API key account: project and location are
// given together (the full address) or not at all (the express address).
func validateAPIKeyAccount(creds, settings object) *pluginv1.FieldError {
	switch key := creds.str(fieldAPIKey); {
	case key == "":
		return fieldError(fieldAPIKey, "required", "API key is required / 请填写 API Key")
	case !apiKeyPattern.MatchString(key):
		return fieldError(fieldAPIKey, "pattern", "API key must be 8-512 visible characters / API Key 必须是 8–512 个可见字符")
	}
	return readTarget(settings).check(true)
}

// ---------------------------------------------------------------- stored accounts

// account is what a request needs of a stored account.
type account struct {
	typ    string
	creds  object
	target target
}

// readAccount decodes an account of one of the plugin's types and checks
// that requests can be addressed for it.
func readAccount(acc *pluginv1.Account) (account, error) {
	a := account{typ: acc.GetType()}
	_, keyed := keyKinds[a.typ]
	if !keyed && a.typ != TypeAPIKey {
		return a, status.Errorf(codes.FailedPrecondition, "account %d: unsupported account type %q", acc.GetId(), a.typ)
	}
	creds, err := decodeObject(acc.GetCredentialsJson())
	if err != nil {
		return a, status.Errorf(codes.InvalidArgument, "account %d: credentials are not a JSON object", acc.GetId())
	}
	settings, err := decodeObject(acc.GetSettingsJson())
	if err != nil {
		return a, status.Errorf(codes.InvalidArgument, "account %d: settings are not a JSON object", acc.GetId())
	}
	a.creds, a.target = creds, readTarget(settings)
	if bad := a.target.check(!keyed); bad != nil {
		return a, status.Errorf(codes.FailedPrecondition, "account %d: %s: %s", acc.GetId(), bad.GetField(), bad.GetMessage())
	}
	return a, nil
}

// keyFile parses the key file of a key file account.
func (a account) keyFile() (keyFile, error) {
	kind, keyed := keyKinds[a.typ]
	if !keyed {
		return nil, status.Errorf(codes.InvalidArgument, "account type %q has no credentials to refresh", a.typ)
	}
	key, err := kind.parse(a.creds.str(kind.field))
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, kind.field+": "+err.Error())
	}
	return key, nil
}
