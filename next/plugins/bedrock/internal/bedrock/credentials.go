package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/reqsign"
)

// Credential and settings fields (forms/*.schema.json).
const (
	fieldAPIKey          = "api_key"
	fieldAccessKeyID     = "access_key_id"
	fieldSecretAccessKey = "secret_access_key"
	fieldSessionToken    = "session_token"
	fieldRegion          = "region"
	fieldProfilePrefix   = "inference_profile_prefix"
	// fieldSessionExpiresAt is a reminder for the administrator only: nothing
	// reads it to decide anything.
	fieldSessionExpiresAt = "session_expires_at"
)

var (
	// regionPattern is strict because the region becomes part of the host.
	regionPattern      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`)
	accessKeyIDPattern = regexp.MustCompile(`^[A-Z0-9]{16,128}$`)
	visiblePattern     = regexp.MustCompile(`^[!-~]+$`)
)

// visible accepts a secret pasted whole: min to max visible characters.
func visible(min, max int) func(string) bool {
	return func(s string) bool { return len(s) >= min && len(s) <= max && visiblePattern.MatchString(s) }
}

// The shapes of the credentials (the same bounds as forms/*.schema.json).
var (
	validAPIKey       = visible(8, 2048)
	validAccessKeyID  = accessKeyIDPattern.MatchString
	validSecretKey    = visible(1, 1024)
	validSessionToken = visible(1, 8192)
)

// NoProfilePrefix is the inference_profile_prefix of an account that sends
// model ids as they are; an absent value means the same.
const NoProfilePrefix = "none"

// ProfilePrefixes are the values of inference_profile_prefix: NoProfilePrefix
// or the cross-region inference prefix put before model ids.
var ProfilePrefixes = []string{NoProfilePrefix, "us", "eu", "apac", "jp", "au", "global"}

// auth is how the accounts of a type authorize their requests.
type auth struct {
	// signed: the core signs the request with the account's access key
	// (SigV4). Otherwise the account's API key is sent as a bearer token.
	signed bool
	// session: the access key is temporary and comes with a session token.
	session bool
}

var auths = map[string]auth{
	TypeAPIKey:    {},
	TypeAccessKey: {signed: true},
	TypeSTS:       {signed: true, session: true},
}

// object is a decoded JSON object.
type object map[string]any

// decodeObject decodes a JSON object; blank input is an empty one.
func decodeObject(raw string) (object, error) {
	o := object{}
	if strings.TrimSpace(raw) == "" {
		return o, nil
	}
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
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

func fieldError(field, code, msg string) *pluginv1.FieldError {
	return &pluginv1.FieldError{Field: field, Code: code, Message: msg}
}

// credential checks a credential field that must be present and valid.
func credential(creds object, field string, valid func(string) bool, required, malformed string) *pluginv1.FieldError {
	switch v := creds.str(field); {
	case v == "":
		return fieldError(field, "required", required)
	case !valid(v):
		return fieldError(field, "pattern", malformed)
	}
	return nil
}

// checkCredentials checks the credential fields of a type.
func (a auth) checkCredentials(creds object) *pluginv1.FieldError {
	if !a.signed {
		return credential(creds, fieldAPIKey, validAPIKey,
			"API key is required / 请填写 API Key", "API key must be 8-2048 visible characters / API Key 必须是 8–2048 个可见字符")
	}
	if bad := credential(creds, fieldAccessKeyID, validAccessKeyID,
		"access key ID is required / 请填写 Access Key ID", "not an AWS access key ID (16-128 upper-case letters and digits) / 不是合法的 AWS Access Key ID（16–128 位大写字母和数字）"); bad != nil {
		return bad
	}
	if bad := credential(creds, fieldSecretAccessKey, validSecretKey,
		"secret access key is required / 请填写 Secret Access Key", "secret access key must not contain spaces or line breaks / Secret Access Key 不能包含空格或换行"); bad != nil {
		return bad
	}
	if !a.session {
		return nil
	}
	return credential(creds, fieldSessionToken, validSessionToken,
		"session token is required / 请填写 Session Token", "session token must not contain spaces or line breaks / Session Token 不能包含空格或换行")
}

// checkSettings checks the settings every type shares and fills in the
// defaults: DefaultRegion and NoProfilePrefix.
func checkSettings(settings object) *pluginv1.FieldError {
	for field, def := range map[string]string{fieldRegion: DefaultRegion, fieldProfilePrefix: NoProfilePrefix} {
		if settings.str(field) == "" {
			settings[field] = def
		}
	}
	switch {
	case !regionPattern.MatchString(settings.str(fieldRegion)):
		return fieldError(fieldRegion, "pattern", "not an AWS region, e.g. us-east-1 / 不是合法的 AWS 区域，例如 us-east-1")
	case !slices.Contains(ProfilePrefixes, settings.str(fieldProfilePrefix)):
		return fieldError(fieldProfilePrefix, "enum", "unknown inference profile prefix / 未知的跨区域推理前缀")
	}
	return nil
}

// checkExpiryHint checks the expiry an administrator noted for temporary
// credentials. Only a submitted form is held to it: a stored account serves
// requests whatever the hint says.
func checkExpiryHint(settings object) *pluginv1.FieldError {
	at := settings.str(fieldSessionExpiresAt)
	if at == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		return fieldError(fieldSessionExpiresAt, "format", "not a date and time / 不是合法的日期时间")
	}
	return nil
}

// check validates what requests are built from: the credentials of an
// account of type typ, its region and its prefix. It is the same for a
// submitted form and for a stored account about to serve a request.
func check(typ string, creds, settings object) (auth, *pluginv1.FieldError) {
	a, ok := auths[typ]
	if !ok {
		return a, fieldError("account_type", "unsupported", "unsupported account type "+strconv.Quote(typ)+" / 不支持的账号类型")
	}
	if bad := a.checkCredentials(creds); bad != nil {
		return a, bad
	}
	return a, checkSettings(settings)
}

// ValidateCredentials implements pluginsdk.Platform. An account without a
// region or a prefix choice is stored with the defaults.
func (p *Plugin) ValidateCredentials(_ context.Context, in *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
	creds, err := decodeObject(in.GetCredentialsJson())
	if err != nil {
		return invalid(fieldError("", "invalid", "credentials must be a JSON object / 凭证必须是 JSON 对象")), nil
	}
	settings, err := decodeObject(in.GetSettingsJson())
	if err != nil {
		return invalid(fieldError("", "invalid", "settings must be a JSON object / 设置必须是 JSON 对象")), nil
	}
	before := settings.json()
	_, bad := check(in.GetAccountType(), creds, settings)
	if bad == nil {
		bad = checkExpiryHint(settings)
	}
	if bad != nil {
		return invalid(bad), nil
	}
	resp := &pluginv1.ValidateCredentialsResponse{}
	if after := settings.json(); !bytes.Equal(before, after) {
		resp.NormalizedSettingsJson = string(after)
	}
	return resp, nil
}

func invalid(e *pluginv1.FieldError) *pluginv1.ValidateCredentialsResponse {
	return &pluginv1.ValidateCredentialsResponse{Errors: []*pluginv1.FieldError{e}}
}

// account is what a request needs of a stored account.
type account struct {
	auth   auth
	creds  object
	region string
	// prefix is the inference profile prefix put before model ids
	// (NoProfilePrefix: none).
	prefix string
}

// readAccount decodes an account of one of the plugin's types. An account
// requests cannot be built for answers FAILED_PRECONDITION: the gateway moves
// on to another account without penalizing this one.
func readAccount(acc *pluginv1.Account) (account, error) {
	creds, err := decodeObject(acc.GetCredentialsJson())
	if err != nil {
		return account{}, status.Errorf(codes.InvalidArgument, "account %d: credentials are not a JSON object", acc.GetId())
	}
	settings, err := decodeObject(acc.GetSettingsJson())
	if err != nil {
		return account{}, status.Errorf(codes.InvalidArgument, "account %d: settings are not a JSON object", acc.GetId())
	}
	a, bad := check(acc.GetType(), creds, settings)
	if bad != nil {
		return account{}, status.Errorf(codes.FailedPrecondition, "account %d: %s: %s", acc.GetId(), bad.GetField(), bad.GetMessage())
	}
	return account{auth: a, creds: creds, region: settings.str(fieldRegion), prefix: settings.str(fieldProfilePrefix)}, nil
}

// bearer is the Authorization value of an API key account.
func (a account) bearer() string { return "Bearer " + a.creds.str(fieldAPIKey) }

// directive is how the requests of an access key account are signed.
func (a account) directive() reqsign.Directive {
	d := reqsign.Directive{
		Scheme: reqsign.SchemeAWSSigV4, Region: a.region, Service: SigningService,
		AccessKeyID: a.creds.str(fieldAccessKeyID), SecretAccessKey: a.creds.str(fieldSecretAccessKey),
	}
	if a.auth.session {
		d.SessionToken = a.creds.str(fieldSessionToken)
	}
	return d
}
