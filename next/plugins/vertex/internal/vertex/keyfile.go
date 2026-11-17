package vertex

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"time"
)

const (
	// CloudPlatformScope is the OAuth scope of the access tokens.
	CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	// assertionLifetime is how long a signed assertion is valid (Google's
	// maximum).
	assertionLifetime = time.Hour
	// assertionBackdate is how far before now an assertion is issued, so a
	// node whose clock runs a few seconds fast is not refused for an iat in
	// the future (golang.org/x/oauth2/jwt does the same).
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
	TokenURI     string `json:"token_uri"`

	signer *rsa.PrivateKey
}

func parseServiceAccount(raw string) (keyFile, error) {
	var k serviceAccountKey
	if json.Unmarshal([]byte(raw), &k) != nil {
		return nil, errNotJSON
	}
	switch {
	case k.Type != "service_account":
		return nil, errors.New(`"type" must be "service_account" / "type" 必须是 "service_account"`)
	case strings.TrimSpace(k.ClientEmail) == "":
		return nil, errors.New(`"client_email" is missing / 缺少 "client_email"`)
	case strings.TrimSpace(k.PrivateKey) == "":
		return nil, errors.New(`"private_key" is missing / 缺少 "private_key"`)
	case k.TokenURI != TokenURL:
		return nil, errors.New(`"token_uri" must be ` + TokenURL + ` / "token_uri" 必须是 ` + TokenURL)
	}
	signer, err := parseRSAPrivateKey(k.PrivateKey)
	if err != nil {
		return nil, err
	}
	k.signer = signer
	return &k, nil
}

func (k *serviceAccountKey) fingerprint() string { return fingerprint(k.ClientEmail, k.PrivateKeyID) }

func (k *serviceAccountKey) project() string { return strings.TrimSpace(k.ProjectID) }

// grant signs the assertion Google exchanges for an access token (RFC 7523):
// issued by the service account for the token endpoint assertionBackdate
// ago, valid one hour from then.
func (k *serviceAccountKey) grant(now time.Time) (url.Values, error) {
	issued := now.Add(-assertionBackdate)
	assertion, err := signRS256(k.signer, k.PrivateKeyID, map[string]any{
		"iss":   k.ClientEmail,
		"scope": CloudPlatformScope,
		"aud":   TokenURL,
		"iat":   issued.Unix(),
		"exp":   issued.Add(assertionLifetime).Unix(),
	})
	if err != nil {
		return nil, err
	}
	return url.Values{"grant_type": {grantJWTBearer}, "assertion": {assertion}}, nil
}

// errPrivateKey carries no detail either: the input is the private key.
var errPrivateKey = errors.New(`"private_key" is not a PEM-encoded RSA private key / "private_key" 不是 PEM 格式的 RSA 私钥`)

// parseRSAPrivateKey reads a PEM block holding a PKCS #8 (what Google
// issues) or PKCS #1 RSA private key.
func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
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

// signRS256 returns a compact JWT signed with RSASSA-PKCS1-v1_5 and SHA-256.
// kid, when not empty, names the key in the header.
func signRS256(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	h, _ := json.Marshal(header)
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signed := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("cannot sign with the private key / 无法用私钥签名")
	}
	return signed + "." + enc.EncodeToString(sig), nil
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

func parseAuthorizedUser(raw string) (keyFile, error) {
	var k authorizedUserKey
	if json.Unmarshal([]byte(raw), &k) != nil {
		return nil, errNotJSON
	}
	if k.Type != "authorized_user" {
		return nil, errors.New(`"type" must be "authorized_user" / "type" 必须是 "authorized_user"`)
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
