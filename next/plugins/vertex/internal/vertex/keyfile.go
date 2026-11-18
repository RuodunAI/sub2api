package vertex

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jws"
)

const (
	// CloudPlatformScope is the OAuth scope of the access tokens.
	CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	// assertionLifetime is how long a signed assertion is valid (Google's
	// maximum).
	assertionLifetime = time.Hour
	// assertionBackdate is how far before now an assertion is issued, so a
	// node whose clock runs a few seconds fast is not refused for an iat in
	// the future (golang.org/x/oauth2/jws does the same when it is left to
	// pick the time; here the time is the plugin's clock).
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

// readCredentials has Google's library (golang.org/x/oauth2/google) read raw
// as a credential file of type typ. Only what the library parsed is used:
// the token source it also builds is never asked for a token, since token
// requests are the core's to send (BuildRefreshRequest). Its errors are
// replaced by ones that quote nothing of the file.
func readCredentials(raw string, typ google.CredentialsType) (*google.Credentials, error) {
	creds, err := google.CredentialsFromJSONWithType(context.Background(), []byte(raw), typ, CloudPlatformScope)
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	switch {
	case err == nil:
		return creds, nil
	case errors.As(err, &syntax), errors.As(err, &mismatch):
		return nil, errNotJSON
	}
	return nil, fmt.Errorf(`"type" must be %q / "type" 必须是 %q`, typ, typ)
}

// fingerprint is the SHA-256 of the parts that identify a key.
func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- service account

// serviceAccountKey is a service account key file (IAM > Service accounts >
// Keys > JSON), as Google's library reads it.
type serviceAccountKey struct {
	email     string
	keyID     string
	projectID string
	signer    *rsa.PrivateKey
}

func parseServiceAccount(raw string) (keyFile, error) {
	creds, err := readCredentials(raw, google.ServiceAccount)
	if err != nil {
		return nil, err
	}
	cfg, err := google.JWTConfigFromJSON(creds.JSON)
	if err != nil {
		return nil, errNotJSON
	}
	switch {
	case strings.TrimSpace(cfg.Email) == "":
		return nil, errors.New(`"client_email" is missing / 缺少 "client_email"`)
	case strings.TrimSpace(string(cfg.PrivateKey)) == "":
		return nil, errors.New(`"private_key" is missing / 缺少 "private_key"`)
	// The file's own value is read: the library fills in its default for a
	// file without token_uri. The value is only checked, never used.
	case gjson.GetBytes(creds.JSON, "token_uri").String() != TokenURL:
		return nil, errors.New(`"token_uri" must be ` + TokenURL + ` / "token_uri" 必须是 ` + TokenURL)
	}
	signer, err := parseRSAPrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &serviceAccountKey{email: cfg.Email, keyID: cfg.PrivateKeyID, projectID: creds.ProjectID, signer: signer}, nil
}

func (k *serviceAccountKey) fingerprint() string { return fingerprint(k.email, k.keyID) }

func (k *serviceAccountKey) project() string { return strings.TrimSpace(k.projectID) }

// grant signs the assertion Google exchanges for an access token (RFC 7523):
// issued by the service account for the token endpoint assertionBackdate
// ago, valid one hour from then. The JWT is encoded and signed (RS256) by
// golang.org/x/oauth2/jws, as in Google's own token source; kid names the
// key when the file has one.
func (k *serviceAccountKey) grant(now time.Time) (url.Values, error) {
	issued := now.Add(-assertionBackdate)
	assertion, err := jws.Encode(
		&jws.Header{Algorithm: "RS256", Typ: "JWT", KeyID: k.keyID},
		&jws.ClaimSet{
			Iss:   k.email,
			Scope: CloudPlatformScope,
			Aud:   TokenURL,
			Iat:   issued.Unix(),
			Exp:   issued.Add(assertionLifetime).Unix(),
		}, k.signer)
	if err != nil {
		return nil, errors.New("cannot sign with the private key / 无法用私钥签名")
	}
	return url.Values{"grant_type": {grantJWTBearer}, "assertion": {assertion}}, nil
}

// errPrivateKey carries no detail either: the input is the private key.
var errPrivateKey = errors.New(`"private_key" is not a PEM-encoded RSA private key / "private_key" 不是 PEM 格式的 RSA 私钥`)

// parseRSAPrivateKey reads a PEM block holding a PKCS #8 (what Google
// issues) or PKCS #1 RSA private key. Google's library keeps its own key
// parser internal (it runs inside the token source), so the standard
// library's is called here.
func parseRSAPrivateKey(pemText []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemText)
	if block == nil {
		return nil, errPrivateKey
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errPrivateKey
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errPrivateKey
	}
	return key, nil
}

// ---------------------------------------------------------------- authorized user

// authorizedUserKey is an application default credentials file of a user
// (gcloud auth application-default login).
type authorizedUserKey struct {
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RefreshToken   string `json:"refresh_token"`
	QuotaProjectID string `json:"quota_project_id"`
}

// parseAuthorizedUser has Google's library check the file, then reads the
// fields of the refresh grant itself: the library keeps them inside its
// token source, which would send the token request on its own.
func parseAuthorizedUser(raw string) (keyFile, error) {
	creds, err := readCredentials(raw, google.AuthorizedUser)
	if err != nil {
		return nil, err
	}
	var k authorizedUserKey
	if json.Unmarshal(creds.JSON, &k) != nil {
		return nil, errNotJSON
	}
	for _, f := range [][2]string{{"client_id", k.ClientID}, {"client_secret", k.ClientSecret}, {"refresh_token", k.RefreshToken}} {
		if strings.TrimSpace(f[1]) == "" {
			return nil, errors.New(`"` + f[0] + `" is missing / 缺少 "` + f[0] + `"`)
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
