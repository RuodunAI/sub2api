package vertex

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// CloudPlatformScope is the OAuth scope of the access tokens.
	CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	// assertionLifetime is how long a signed assertion is valid (Google's
	// maximum).
	assertionLifetime = time.Hour
	// assertionBackdate is how far before now an assertion is issued, so a
	// node whose clock runs a few seconds fast is not refused for an iat in
	// the future.
	assertionBackdate = 10 * time.Second

	grantJWTBearer    = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	grantRefreshToken = "refresh_token"
)

// keyFile is a Google credential file the token endpoint exchanges for
// access tokens.
type keyFile interface {
	// fingerprint identifies the key. An access token stored with another
	// fingerprint was issued for a key that has since been replaced.
	fingerprint() string
	// project is the project the file names, "" when it names none.
	project() string
	// grant is the form of the token request at now.
	grant(now time.Time) (url.Values, error)
}

// keyKind is how an account type keeps its key file.
type keyKind struct {
	// field is the credentials field holding the file's JSON text.
	field string
	parse func(raw string) (keyFile, error)
	// quotaProject makes requests name the account's project in
	// x-goog-user-project: user credentials belong to no project, and
	// without a quota project aiplatform answers 403.
	quotaProject bool
}

// keyKinds are the account types authorized by a key file, by type id.
var keyKinds = map[string]keyKind{
	TypeServiceAccount: {field: "service_account_json", parse: parseServiceAccount},
	TypeAuthorizedUser: {field: "authorized_user_json", parse: parseAuthorizedUser, quotaProject: true},
}

// errNotJSON deliberately carries no detail of the text: a decoding error
// may quote part of a secret.
var errNotJSON = errors.New("not a JSON object / 不是合法的 JSON 对象")

// decodeKeyFile reads raw, the JSON text of a credential file, into dst and
// checks that the file is of type typ. This one decoding is the only reading
// of the file: every check and every later use sees the same values.
// encoding/json matches key names without regard to case and keeps the last
// of repeated keys, so dst holds the file's effective values whatever its
// layout. Errors name a field at most, never a value.
func decodeKeyFile(raw, typ string, dst interface{ fileType() string }) error {
	var mismatch *json.UnmarshalTypeError
	switch err := json.Unmarshal([]byte(raw), dst); {
	case errors.As(err, &mismatch) && mismatch.Field != "":
		return fmt.Errorf(`%q must be a string / %q 必须是字符串`, mismatch.Field, mismatch.Field)
	case err != nil:
		return errNotJSON
	case dst.fileType() != typ:
		return fmt.Errorf(`"type" must be %q / "type" 必须是 %q`, typ, typ)
	}
	return nil
}

// errMissing reports a field the file must have.
func errMissing(field string) error {
	return fmt.Errorf(`%q is missing / 缺少 %q`, field, field)
}

// fingerprint is the SHA-256 of the parts that identify a key.
func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- service account

// serviceAccountKey is a service account key file (IAM > Service accounts >
// Keys > JSON).
type serviceAccountKey struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	// TokenURI is only checked: token requests always go to TokenURL.
	TokenURI string `json:"token_uri"`

	signer *rsa.PrivateKey
}

func (k *serviceAccountKey) fileType() string { return k.Type }

func parseServiceAccount(raw string) (keyFile, error) {
	var k serviceAccountKey
	if err := decodeKeyFile(raw, "service_account", &k); err != nil {
		return nil, err
	}
	switch {
	case strings.TrimSpace(k.ClientEmail) == "":
		return nil, errMissing("client_email")
	case strings.TrimSpace(k.PrivateKey) == "":
		return nil, errMissing("private_key")
	case k.TokenURI != TokenURL:
		return nil, errors.New(`"token_uri" must be ` + TokenURL + ` / "token_uri" 必须是 ` + TokenURL)
	}
	// PKCS #8 (what Google issues) and PKCS #1 are both read.
	signer, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(k.PrivateKey))
	if err != nil {
		return nil, errPrivateKey
	}
	k.signer = signer
	return &k, nil
}

// errPrivateKey carries no detail either: the input is the private key.
var errPrivateKey = errors.New(`"private_key" is not a PEM-encoded RSA private key / "private_key" 不是 PEM 格式的 RSA 私钥`)

func (k *serviceAccountKey) fingerprint() string { return fingerprint(k.ClientEmail, k.PrivateKeyID) }

func (k *serviceAccountKey) project() string { return strings.TrimSpace(k.ProjectID) }

// grant signs the assertion Google exchanges for an access token (RFC 7523):
// issued by the service account for the token endpoint assertionBackdate
// ago, valid one hour from then. The JWT is encoded and signed (RS256) by
// github.com/golang-jwt/jwt/v5, which checks no claim when signing: the
// times are whole seconds because they are set as such here. kid names the
// key when the file has one.
func (k *serviceAccountKey) grant(now time.Time) (url.Values, error) {
	issued := now.Add(-assertionBackdate)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   k.ClientEmail,
		"scope": CloudPlatformScope,
		"aud":   TokenURL,
		"iat":   issued.Unix(),
		"exp":   issued.Add(assertionLifetime).Unix(),
	})
	if k.PrivateKeyID != "" {
		token.Header["kid"] = k.PrivateKeyID
	}
	assertion, err := token.SignedString(k.signer)
	if err != nil {
		return nil, errors.New("cannot sign with the private key / 无法用私钥签名")
	}
	return url.Values{"grant_type": {grantJWTBearer}, "assertion": {assertion}}, nil
}

// ---------------------------------------------------------------- authorized user

// authorizedUserKey is an application default credentials file of a user
// (gcloud auth application-default login).
type authorizedUserKey struct {
	Type           string `json:"type"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RefreshToken   string `json:"refresh_token"`
	QuotaProjectID string `json:"quota_project_id"`
}

func (k *authorizedUserKey) fileType() string { return k.Type }

func parseAuthorizedUser(raw string) (keyFile, error) {
	var k authorizedUserKey
	if err := decodeKeyFile(raw, "authorized_user", &k); err != nil {
		return nil, err
	}
	for _, f := range [][2]string{{"client_id", k.ClientID}, {"client_secret", k.ClientSecret}, {"refresh_token", k.RefreshToken}} {
		if strings.TrimSpace(f[1]) == "" {
			return nil, errMissing(f[0])
		}
	}
	return &k, nil
}

func (k *authorizedUserKey) fingerprint() string { return fingerprint(k.ClientID, k.RefreshToken) }

func (k *authorizedUserKey) project() string { return strings.TrimSpace(k.QuotaProjectID) }

func (k *authorizedUserKey) grant(time.Time) (url.Values, error) {
	return url.Values{
		"grant_type":    {grantRefreshToken},
		"client_id":     {k.ClientID},
		"client_secret": {k.ClientSecret},
		"refresh_token": {k.RefreshToken},
	}, nil
}
