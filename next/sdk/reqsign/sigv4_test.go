package reqsign

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	suiteAccessKey = "AKIDEXAMPLE"
	suiteSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// suiteRequest parses a .req file of AWS's test suite: request line, headers
// up to an empty line, then the body.
func suiteRequest(t *testing.T, raw string) (req *http.Request, payload []byte, token string) {
	t.Helper()
	head, body, _ := strings.Cut(raw, "\n\n")
	lines := strings.Split(head, "\n")
	method, rest, _ := strings.Cut(lines[0], " ")
	target := strings.TrimSuffix(rest, " HTTP/1.1")
	req, err := http.NewRequest(method, "https://placeholder"+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines[1:] {
		name, value, _ := strings.Cut(line, ":")
		switch strings.ToLower(name) {
		case "host":
			req.Host = value
		case "x-amz-date": // written by the signer from the clock
		case "x-amz-security-token":
			token = value
		default:
			req.Header.Add(name, value)
		}
	}
	return req, []byte(body), token
}

// TestOfficialSuite replays AWS's published SigV4 test suite: the canonical
// request, the string to sign and the Authorization header must all match.
func TestOfficialSuite(t *testing.T) {
	dirs, err := filepath.Glob("testdata/aws-sig-v4-test-suite/*")
	if err != nil || len(dirs) < 11 {
		t.Fatalf("suite cases: %d %v", len(dirs), err)
	}
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	for _, dir := range dirs {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			file := func(ext string) string { return readFile(t, filepath.Join(dir, name+"."+ext)) }
			req, payload, token := suiteRequest(t, file("req"))
			d := Directive{Scheme: SchemeAWSSigV4, Region: "us-east-1", Service: "service",
				AccessKeyID: suiteAccessKey, SecretAccessKey: suiteSecretKey, SessionToken: token}
			if err := Sign(req, payload, d, now); err != nil {
				t.Fatal(err)
			}
			canonical, _ := sigV4CanonicalRequest(req, payload)
			if want := file("creq"); canonical != want {
				t.Errorf("canonical request\n got: %q\nwant: %q", canonical, want)
			}
			sts := sigV4StringToSign("20150830T123600Z", "20150830/us-east-1/service/aws4_request", canonical)
			if want := file("sts"); sts != want {
				t.Errorf("string to sign\n got: %q\nwant: %q", sts, want)
			}
			if got, want := req.Header.Get("Authorization"), file("authz"); got != want {
				t.Errorf("authorization\n got: %s\nwant: %s", got, want)
			}
			if req.Header.Get("X-Amz-Date") != "20150830T123600Z" || req.Header.Get("X-Amz-Security-Token") != token {
				t.Errorf("headers %v", req.Header)
			}
		})
	}
}

type sdkVector struct {
	Name          string            `json:"name"`
	Method        string            `json:"method"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	Body          string            `json:"body"`
	Region        string            `json:"region"`
	Service       string            `json:"service"`
	AccessKeyID   string            `json:"access_key_id"`
	SecretKey     string            `json:"secret_access_key"`
	SessionToken  string            `json:"session_token"`
	Time          time.Time         `json:"time"`
	Authorization string            `json:"authorization"`
	AmzDate       string            `json:"x_amz_date"`
}

func sdkVectors(t *testing.T) []sdkVector {
	t.Helper()
	var vs []sdkVector
	if err := json.Unmarshal([]byte(readFile(t, "testdata/aws-sdk-go-v2.json")), &vs); err != nil || len(vs) < 8 {
		t.Fatalf("vectors: %d %v", len(vs), err)
	}
	return vs
}

func (v sdkVector) request(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(v.Method, v.URL, strings.NewReader(v.Body))
	if err != nil {
		t.Fatal(err)
	}
	for k, val := range v.Headers {
		req.Header.Set(k, val)
	}
	return req
}

func (v sdkVector) directive() Directive {
	return Directive{Scheme: SchemeAWSSigV4, Region: v.Region, Service: v.Service,
		AccessKeyID: v.AccessKeyID, SecretAccessKey: v.SecretKey, SessionToken: v.SessionToken}
}

// TestSDKRecordings compares with what aws-sdk-go-v2 produced for
// Bedrock-shaped requests (testdata/README.md).
func TestSDKRecordings(t *testing.T) {
	for _, v := range sdkVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			req := v.request(t)
			if err := Sign(req, []byte(v.Body), v.directive(), v.Time); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Authorization"); got != v.Authorization {
				canonical, _ := sigV4CanonicalRequest(req, []byte(v.Body))
				t.Errorf("authorization\n got: %s\nwant: %s\ncanonical request:\n%s", got, v.Authorization, canonical)
			}
			if req.Header.Get("X-Amz-Date") != v.AmzDate || req.Header.Get("X-Amz-Security-Token") != v.SessionToken {
				t.Errorf("headers %v", req.Header)
			}
		})
	}
}

// The wire path keeps ":" as "%3A" and the canonical path escapes it again.
func TestCanonicalPathIsEscapedTwice(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-v1%3A0/invoke", nil)
	canonical, _ := sigV4CanonicalRequest(req, nil)
	if got := strings.Split(canonical, "\n")[1]; got != "/model/anthropic.claude-v1%253A0/invoke" {
		t.Fatalf("canonical path %q", got)
	}
	if got := req.URL.EscapedPath(); got != "/model/anthropic.claude-v1%3A0/invoke" {
		t.Fatalf("wire path %q", got)
	}
}

func signed(t *testing.T, v sdkVector, edit func(*http.Request, *[]byte, *Directive, *time.Time)) string {
	t.Helper()
	req, payload, d, now := v.request(t), []byte(v.Body), v.directive(), v.Time
	if edit != nil {
		edit(req, &payload, &d, &now)
	}
	if err := Sign(req, payload, d, now); err != nil {
		t.Fatal(err)
	}
	return req.Header.Get("Authorization")
}

// What is signed changes the signature; what is not signed does not.
func TestSignatureCoverage(t *testing.T) {
	v := sdkVectors(t)[0]
	base := signed(t, v, nil)
	if base != v.Authorization {
		t.Fatalf("base %s", base)
	}
	changes := map[string]func(*http.Request, *[]byte, *Directive, *time.Time){
		"one body byte": func(_ *http.Request, p *[]byte, _ *Directive, _ *time.Time) { (*p)[len(*p)-1] ^= 1 },
		"empty body":    func(_ *http.Request, p *[]byte, _ *Directive, _ *time.Time) { *p = nil },
		"content-type": func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) {
			r.Header.Set("Content-Type", "text/plain")
		},
		"host": func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) { r.Host = "other.example.com" },
		"path": func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) {
			r.URL.Path, r.URL.RawPath = "/model/x/invoke", ""
		},
		"query":            func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) { r.URL.RawQuery = "a=1" },
		"method":           func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) { r.Method = http.MethodPut },
		"time":             func(_ *http.Request, _ *[]byte, _ *Directive, n *time.Time) { *n = n.Add(time.Second) },
		"region":           func(_ *http.Request, _ *[]byte, d *Directive, _ *time.Time) { d.Region = "eu-west-1" },
		"service":          func(_ *http.Request, _ *[]byte, d *Directive, _ *time.Time) { d.Service = "s3" },
		"secret":           func(_ *http.Request, _ *[]byte, d *Directive, _ *time.Time) { d.SecretAccessKey += "x" },
		"session token":    func(_ *http.Request, _ *[]byte, d *Directive, _ *time.Time) { d.SessionToken = "tok" },
		"request.Host set": func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) { r.Host = "h.example.com:8443" },
	}
	for name, edit := range changes {
		if got := signed(t, v, edit); got == base {
			t.Errorf("%s: signature unchanged", name)
		}
	}
	same := map[string]func(*http.Request, *[]byte, *Directive, *time.Time){
		"unsigned headers": func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) {
			r.Header.Set("Accept", "application/json")
			r.Header.Set("User-Agent", "x")
			r.Header.Set("X-Amzn-Bedrock-Trace", "ENABLED")
		},
		"stale credential headers": func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) {
			r.Header.Set("Authorization", "Bearer stale")
			r.Header.Set("X-Amz-Date", "20000101T000000Z")
			r.Header.Set("X-Amz-Security-Token", "stale")
		},
		"header value spacing": func(r *http.Request, _ *[]byte, _ *Directive, _ *time.Time) {
			r.Header.Set("Content-Type", "  application/json ")
		},
		"local time zone": func(_ *http.Request, _ *[]byte, _ *Directive, n *time.Time) {
			*n = n.In(time.FixedZone("x", 8*3600))
		},
	}
	for name, edit := range same {
		if got := signed(t, v, edit); got != base {
			t.Errorf("%s: signature changed", name)
		}
	}
}

// Sign replaces credentials an earlier layer wrote and removes a session
// token header the directive does not carry.
func TestSignReplacesCredentialHeaders(t *testing.T) {
	v := sdkVectors(t)[0]
	req := v.request(t)
	req.Header.Set("Authorization", "Bearer stale")
	req.Header.Set("X-Amz-Security-Token", "stale")
	if err := Sign(req, []byte(v.Body), v.directive(), v.Time); err != nil {
		t.Fatal(err)
	}
	if len(req.Header.Values("Authorization")) != 1 || !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") ||
		req.Header.Get("X-Amz-Security-Token") != "" {
		t.Fatalf("headers %v", req.Header)
	}
}

func TestSignErrors(t *testing.T) {
	v := sdkVectors(t)[0]
	const secret, token = "s3cr3t-value", "t0ken-value"
	for name, d := range map[string]Directive{
		"unknown scheme": {Scheme: "hmac-v9", Region: "r", Service: "s", AccessKeyID: "AK", SecretAccessKey: secret, SessionToken: token},
		"empty scheme":   {Region: "r", Service: "s", AccessKeyID: "AK", SecretAccessKey: secret, SessionToken: token},
		"no region":      {Scheme: SchemeAWSSigV4, Service: "s", AccessKeyID: "AK", SecretAccessKey: secret, SessionToken: token},
		"no service":     {Scheme: SchemeAWSSigV4, Region: "r", AccessKeyID: "AK", SecretAccessKey: secret, SessionToken: token},
		"no access key":  {Scheme: SchemeAWSSigV4, Region: "r", Service: "s", SecretAccessKey: secret, SessionToken: token},
		"no secret":      {Scheme: SchemeAWSSigV4, Region: "r", Service: "s", AccessKeyID: "AK", SessionToken: token},
	} {
		req := v.request(t)
		req.Header.Set("Authorization", "Bearer kept")
		err := Sign(req, []byte(v.Body), d, v.Time)
		if err == nil {
			t.Errorf("%s: signed", name)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), token) {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
		if req.Header.Get("Authorization") != "Bearer kept" || req.Header.Get("X-Amz-Date") != "" {
			t.Errorf("%s: request touched: %v", name, req.Header)
		}
		if unknown := strings.HasSuffix(name, "scheme"); errors.Is(err, ErrUnknownScheme) != unknown {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := Sign(nil, nil, v.directive(), v.Time); err == nil {
		t.Error("nil request signed")
	}
	if !Known(SchemeAWSSigV4) || Known("") || Known("hmac-v9") {
		t.Error("Known")
	}
}

// A directive that ends up in a log line or an error by accident shows
// neither the secret nor the session token.
func TestDirectiveIsRedacted(t *testing.T) {
	d := Directive{Scheme: SchemeAWSSigV4, Region: "us-east-1", Service: "bedrock", AccessKeyID: "AKIDEXAMPLE",
		SecretAccessKey: "s3cr3t-value", SessionToken: "t0ken-value"}
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("x", "directive", d, "pointer", &d)
	out := buf.String() + fmt.Sprintf("%v %+v %#v %s %v", d, d, d, d, &d) + fmt.Errorf("wrap: %v", d).Error()
	if strings.Contains(out, "s3cr3t-value") || strings.Contains(out, "t0ken-value") {
		t.Fatalf("leak: %s", out)
	}
	if !strings.Contains(out, "AKIDEXAMPLE") || !strings.Contains(out, "bedrock") {
		t.Fatalf("not descriptive: %s", out)
	}
}
