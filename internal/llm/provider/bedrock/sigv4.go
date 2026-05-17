// Package bedrock — minimal SigV4 helper.
//
// Implements just enough of AWS Signature Version 4 to sign a single
// POST request to a Bedrock Converse endpoint. No external AWS SDK
// dependency — the whole flow is ~80 LOC of stdlib code. Multi-region
// requests are supported (region is supplied per-call). The signer
// honours an optional STS session token (carried as the
// `x-amz-security-token` header) so STS-issued credentials work out
// of the box.
package bedrock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// sigV4Creds carries the per-call credentials. SessionToken may be
// empty for long-lived IAM users.
type sigV4Creds struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Region       string
	Service      string // always "bedrock" here
}

// sign computes the SigV4 Authorization header and attaches it to req
// (plus the x-amz-date / x-amz-content-sha256 / x-amz-security-token
// headers). The body argument is the raw payload bytes — pass an empty
// slice for GETs. now is the request timestamp; tests pin it for
// reproducibility.
func sign(req *http.Request, body []byte, creds sigV4Creds, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := hex.EncodeToString(sha256Sum(body))

	req.Header.Set("host", req.URL.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.SessionToken)
	}

	// Canonical headers: all signed headers, lowercased name, trimmed
	// value, sorted by name, each followed by \n.
	type kv struct{ name, value string }
	var signed []kv
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" {
			continue
		}
		signed = append(signed, kv{lower, strings.TrimSpace(values[0])})
	}
	sort.Slice(signed, func(i, j int) bool { return signed[i].name < signed[j].name })

	var canonicalHeaders strings.Builder
	var signedHeaderNames []string
	for _, h := range signed {
		canonicalHeaders.WriteString(h.name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(h.value)
		canonicalHeaders.WriteByte('\n')
		signedHeaderNames = append(signedHeaderNames, h.name)
	}
	signedHeaders := strings.Join(signedHeaderNames, ";")

	// Non-S3 services require the canonical URI to be URL-encoded a
	// second time on top of the already-encoded request path. Bedrock
	// model IDs contain `:` which is sent as `%3A` and must become
	// `%253A` here.
	canonicalRequest := strings.Join([]string{
		req.Method,
		doubleEscapePath(req.URL.EscapedPath()),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := dateStamp + "/" + creds.Region + "/" + creds.Service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+creds.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, creds.Region)
	kService := hmacSHA256(kRegion, creds.Service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	auth := "AWS4-HMAC-SHA256 Credential=" + creds.AccessKey + "/" + credentialScope +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + signature
	req.Header.Set("authorization", auth)
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// doubleEscapePath URL-encodes each `/`-separated segment of an
// already-encoded path string a second time. Bedrock model IDs ship
// `:` as `%3A` in the request URL, which must become `%253A` in the
// canonical URI used for signing.
func doubleEscapePath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
