package reqsign

// AWS Signature Version 4, header authentication, for services other than S3
// (the canonical path is escaped twice and the payload is always hashed).
//
// The rules below are those of aws-sdk-go-v2's aws/signer/v4, which the
// golden vectors in testdata were recorded from, next to AWS's published
// test suite:
//
//   - signed headers: host and x-amz-date, plus content-type and
//     x-amz-security-token when present. Nothing else is signed, so headers
//     added or changed by other layers do not invalidate the signature;
//   - canonical path: every segment of the path as it goes on the wire
//     (URL.EscapedPath) is escaped once more with the RFC 3986 unreserved
//     set, so ":" sent as "%3A" is signed as "%253A";
//   - canonical query: sorted by key, then value, percent-encoded with the
//     unreserved set (a space is "%20", never "+"). URL.RawQuery is rewritten
//     to that string, so the query sent is the query signed; one that cannot
//     be parsed is an error, never signed in part;
//   - host: Request.Host, else URL.Host, without the scheme's default port.
//     It is written back to Request.Host, so the Host header sent is the
//     host signed.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	sigV4TimeFormat = "20060102T150405Z"
	sigV4Terminator = "aws4_request"
)

func signAWSSigV4(req *http.Request, payload []byte, d Directive, now time.Time) error {
	if d.AccessKeyID == "" || d.SecretAccessKey == "" || d.Region == "" || d.Service == "" {
		return errors.New("reqsign: aws-sigv4 needs region, service, access key id and secret access key")
	}
	query, err := sigV4Query(req.URL.RawQuery)
	if err != nil {
		return err
	}
	// What goes on the wire is exactly what is signed.
	req.URL.RawQuery = query
	req.Host = sigV4Host(req)

	amzDate := now.UTC().Format(sigV4TimeFormat)
	date := amzDate[:8]

	// Whatever credential an earlier layer wrote is replaced, never signed
	// over or sent next to this one.
	req.Header.Del("Authorization")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Del("X-Amz-Security-Token")
	if d.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", d.SessionToken)
	}

	canonical, signedHeaders := sigV4CanonicalRequest(req, payload)
	scope := strings.Join([]string{date, d.Region, d.Service, sigV4Terminator}, "/")
	stringToSign := sigV4StringToSign(amzDate, scope, canonical)

	key := []byte("AWS4" + d.SecretAccessKey)
	for _, part := range []string{date, d.Region, d.Service, sigV4Terminator} {
		key = hmacSHA256(key, part)
	}
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", sigV4Algorithm+
		" Credential="+d.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
	return nil
}

// sigV4CanonicalRequest is the canonical form of req with the body payload,
// and the list of headers it signs. The query of req is canonical already
// (sigV4Query).
func sigV4CanonicalRequest(req *http.Request, payload []byte) (canonical, signedHeaders string) {
	names, headers := sigV4Headers(req)
	signedHeaders = strings.Join(names, ";")
	return strings.Join([]string{
		req.Method,
		sigV4Path(req.URL.EscapedPath()),
		req.URL.RawQuery,
		headers,
		signedHeaders,
		sha256Hex(payload),
	}, "\n"), signedHeaders
}

func sigV4StringToSign(amzDate, scope, canonical string) string {
	return strings.Join([]string{sigV4Algorithm, amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
}

// sigV4Headers returns the sorted names of the signed headers and their
// canonical "name:value\n" lines.
func sigV4Headers(req *http.Request) (names []string, canonical string) {
	values := map[string]string{"host": sigV4Host(req)}
	for _, name := range []string{"Content-Type", "X-Amz-Date", "X-Amz-Security-Token"} {
		if vs := req.Header.Values(name); len(vs) > 0 {
			values[strings.ToLower(name)] = strings.Join(vs, ",")
		}
	}
	names = make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		// Surrounding space is dropped and inner runs collapse to one.
		b.WriteString(name + ":" + strings.Join(strings.Fields(values[name]), " ") + "\n")
	}
	return names, b.String()
}

// sigV4Query is the canonical form of a raw query string: parameters sorted
// by key, then by value, each percent-encoded with the unreserved set. A
// query net/url cannot parse (a ";" separator, a bad escape) is an error: its
// parameters would otherwise be left out of the signature and still be sent.
func sigV4Query(raw string) (string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", fmt.Errorf("reqsign: aws-sigv4 cannot parse the query string: %w", err)
	}
	for _, vs := range values {
		sort.Strings(vs)
	}
	return strings.ReplaceAll(values.Encode(), "+", "%20"), nil // Encode sorts the keys
}

// sigV4Host is the authority the request is addressed to, without the port
// when it is the scheme's default: that is how the AWS side canonicalises it.
func sigV4Host(req *http.Request) string {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	defaultPort := map[string]string{"http": ":80", "https": ":443"}[req.URL.Scheme]
	if defaultPort != "" {
		host = strings.TrimSuffix(host, defaultPort)
	}
	return host
}

// sigV4Path escapes each segment of the already escaped wire path once more.
func sigV4Path(escaped string) string {
	if escaped == "" {
		return "/"
	}
	parts := strings.Split(escaped, "/")
	for i, part := range parts {
		parts[i] = escapeUnreserved(part)
	}
	return strings.Join(parts, "/")
}

// escapeUnreserved percent-encodes everything outside the RFC 3986
// unreserved set: url.QueryEscape does, except that it writes a space as "+".
// net/url's PathEscape is not equivalent: it leaves sub-delimiters such as
// "$&+,:;=@" alone.
func escapeUnreserved(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func hmacSHA256(key []byte, content string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(content))
	return mac.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
