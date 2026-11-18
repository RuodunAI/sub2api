package reqsign

// AWS Signature Version 4, header authentication, for services other than S3
// (the canonical path is escaped twice and the payload is always hashed).
//
// The signature is made by the official signer, aws-sdk-go-v2's
// aws/signer/v4: the canonical request, the string to sign, the key
// derivation and the Authorization header are its work. What is left here is
// the choice of what it gets to sign, and two checks it does not make:
//
//   - signed headers: host and x-amz-date, plus content-type and
//     x-amz-security-token when present. The SDK signer signs every header
//     on the request it is handed (but for a short ignore list) and
//     content-length. A request of the gateway also carries what the client
//     sent and what the plugin added, and layers after the signature (header
//     overrides, the proxy, net/http itself) may still change those: signing
//     them would turn any such change into a 403. So the signer is handed a
//     bare copy of the request with only Content-Type on it, and its three
//     result headers are copied back;
//   - canonical path: every segment of the path as it goes on the wire
//     (URL.EscapedPath) is escaped once more with the RFC 3986 unreserved
//     set, so ":" sent as "%3A" is signed as "%253A" (the signer's default,
//     DisableURIPathEscaping off);
//   - canonical query: each name and value percent-encoded with the
//     unreserved set (a space is "%20", never "+"), then sorted by name and
//     value. URL.RawQuery is rewritten to that string, so the query sent is
//     the query signed. A query that cannot be parsed is an error here: the
//     signer would sign and send it without the parameters net/url drops;
//   - host: Request.Host, else URL.Host, without the scheme's default port.
//     A host that lost its port is written back to Request.Host, so the Host
//     header sent is the host signed. (The signer also drops the brackets of
//     an IPv6 literal it takes the default port off; AWS endpoints are
//     names.)

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// sigV4ResultHeaders are the headers the signer writes. Whatever an earlier
// layer put in them is replaced, never signed over or sent next to the new
// values.
var sigV4ResultHeaders = []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token"}

func signAWSSigV4(req *http.Request, payload []byte, d Directive, now time.Time) error {
	return sigV4Sign(req, payload, d, now)
}

// sigV4Sign signs req with the SDK signer; opts adjust the signer for this
// one call (the tests have it log the canonical request).
func sigV4Sign(req *http.Request, payload []byte, d Directive, now time.Time, opts ...func(*v4.SignerOptions)) error {
	if d.AccessKeyID == "" || d.SecretAccessKey == "" || d.Region == "" || d.Service == "" {
		return errors.New("reqsign: aws-sigv4 needs region, service, access key id and secret access key")
	}
	if _, err := url.ParseQuery(req.URL.RawQuery); err != nil {
		return fmt.Errorf("reqsign: aws-sigv4 cannot parse the query string: %w", err)
	}
	bare := sigV4BareRequest(req)
	creds := aws.Credentials{AccessKeyID: d.AccessKeyID, SecretAccessKey: d.SecretAccessKey, SessionToken: d.SessionToken}
	sum := sha256.Sum256(payload)
	// A signer of its own for every signature: a shared one caches the
	// derived key per region and service under the access key id alone, and
	// would keep signing with the old secret after an account's secret was
	// corrected.
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, bare, hex.EncodeToString(sum[:]), d.Service, d.Region, now, opts...); err != nil {
		// The signer's own text is left out: nothing here vouches for what
		// it quotes.
		return errors.New("reqsign: aws-sigv4 signing failed")
	}
	// What goes on the wire is exactly what was signed.
	req.URL.RawQuery, req.Host = bare.URL.RawQuery, bare.Host
	for _, name := range sigV4ResultHeaders {
		req.Header.Del(name)
		if v := bare.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	return nil
}

// sigV4BareRequest is the part of req the signature covers besides the
// payload: method, URL, host and Content-Type. The signer works on it, so
// req is untouched until the signature exists.
func sigV4BareRequest(req *http.Request) *http.Request {
	u := *req.URL
	bare := &http.Request{Method: req.Method, URL: &u, Host: req.Host, Header: http.Header{}}
	if ct := req.Header.Values("Content-Type"); len(ct) > 0 {
		bare.Header["Content-Type"] = ct
	}
	return bare
}
