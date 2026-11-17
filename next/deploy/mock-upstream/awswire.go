package main

// The two AWS wire formats the Bedrock paths need, standard library only.
// This module is built on its own (the image's build context is this
// directory), so it cannot import next/sdk; awswire_test.go pins both
// against output of the official AWS SDK.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// appendEventStreamMessage appends one AWS event stream message with string
// headers: total length, headers length, CRC-32 of those two, the headers
// (name length, name, type 7, value length, value), the payload, and the
// CRC-32 of everything before it.
func appendEventStreamMessage(dst []byte, headers [][2]string, payload []byte) []byte {
	var block []byte
	for _, h := range headers {
		block = append(append(block, byte(len(h[0]))), h[0]...)
		block = binary.BigEndian.AppendUint16(append(block, 7), uint16(len(h[1])))
		block = append(block, h[1]...)
	}
	start := len(dst)
	dst = binary.BigEndian.AppendUint32(dst, uint32(16+len(block)+len(payload)))
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(block)))
	dst = binary.BigEndian.AppendUint32(dst, crc32.ChecksumIEEE(dst[start:]))
	dst = append(append(dst, block...), payload...)
	return binary.BigEndian.AppendUint32(dst, crc32.ChecksumIEEE(dst[start:]))
}

// sigV4Authorization matches a Signature Version 4 Authorization header:
// access key id, date, region, service, signed headers, signature.
var sigV4Authorization = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/ ]+)/(\d{8})/([^/ ]+)/([^/ ]+)/aws4_request, ?SignedHeaders=([a-z0-9;-]+), ?Signature=([0-9a-f]{64})$`)

// mockSecretAccessKey is the secret the mock expects for an access key id.
func mockSecretAccessKey(accessKeyID string) string { return "mock-secret-" + accessKeyID }

// verifySigV4 checks the signature of a SigV4 request as AWS would, taking
// the secret of an access key id to be mockSecretAccessKey. A request with
// any other Authorization (a Bedrock API key) is not checked.
func verifySigV4(r *http.Request, body []byte) error {
	m := sigV4Authorization.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return nil
	}
	return checkSigV4(r, body, mockSecretAccessKey(m[1]), time.Now())
}

// checkSigV4 checks r's SigV4 Authorization against secret at the time now.
func checkSigV4(r *http.Request, body []byte, secret string, now time.Time) error {
	m := sigV4Authorization.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return errors.New("the Authorization header is not AWS4-HMAC-SHA256")
	}
	date, signedHeaders, signature := m[2], m[5], m[6]
	amzDate := r.Header.Get("X-Amz-Date")
	at, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || !strings.HasPrefix(amzDate, date) {
		return fmt.Errorf("x-amz-date %q does not fit the credential scope date %s", amzDate, date)
	}
	if age := now.Sub(at); age < -15*time.Minute || age > 15*time.Minute {
		return fmt.Errorf("signature expired: %s is more than 15 minutes from now", amzDate)
	}
	signed := ";" + signedHeaders + ";"
	if !strings.Contains(signed, ";host;") || !strings.Contains(signed, ";x-amz-date;") {
		return errors.New("host and x-amz-date must be signed")
	}
	if r.Header.Get("X-Amz-Security-Token") != "" && !strings.Contains(signed, ";x-amz-security-token;") {
		return errors.New("x-amz-security-token must be signed")
	}
	if want := sigV4Signature(r, body, secret, amzDate, m[3], m[4], signedHeaders); !hmac.Equal([]byte(want), []byte(signature)) {
		return errors.New("the request signature we calculated does not match the signature you provided")
	}
	return nil
}

// sigV4Signature computes the signature of r as received, for the signed
// headers named: canonical request, string to sign, derived key.
func sigV4Signature(r *http.Request, body []byte, secret, amzDate, region, service, signedHeaders string) string {
	var headers strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		value := strings.Join(r.Header.Values(name), ",")
		if name == "host" {
			value = r.Host
		}
		headers.WriteString(name + ":" + strings.Join(strings.Fields(value), " ") + "\n")
	}
	// Every service but S3 escapes the path a second time.
	segments := strings.Split(r.URL.EscapedPath(), "/")
	for i, seg := range segments {
		segments[i] = awsEscape(seg)
	}
	var pairs []string
	for key, values := range r.URL.Query() {
		for _, v := range values {
			pairs = append(pairs, awsEscape(key)+"="+awsEscape(v))
		}
	}
	sort.Strings(pairs)
	canonical := strings.Join([]string{r.Method, strings.Join(segments, "/"), strings.Join(pairs, "&"),
		headers.String(), signedHeaders, sha256Hex(body)}, "\n")
	date := amzDate[:8]
	scope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	key := []byte("AWS4" + secret)
	for _, part := range []string{date, region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	return hex.EncodeToString(hmacSHA256(key, strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")))
}

// awsEscape percent-encodes everything but the RFC 3986 unreserved set.
func awsEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func hmacSHA256(key []byte, s string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(s))
	return mac.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
