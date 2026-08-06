package s3

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SigV4 signing. The algorithm is fully specified by AWS, so the value here
// is in getting the canonicalisation exactly right — that is where
// compatibility with non-AWS implementations is usually lost.
const (
	algorithm  = "AWS4-HMAC-SHA256"
	service    = "s3"
	isoLayout  = "20060102T150405Z"
	dateLayout = "20060102"
	// emptyPayload is the SHA-256 of an empty body, which bodyless requests
	// must present rather than omitting the header.
	emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// signer produces SigV4 signatures. It is split out so the multipart writer
// can sign each part with the same code the single-shot path uses.
type signer struct {
	accessKey string
	secretKey string
	region    string
	now       func() time.Time
}

func (c *Client) signer() *signer {
	return &signer{
		accessKey: c.cfg.AccessKey,
		secretKey: c.cfg.SecretKey,
		region:    c.cfg.Region,
		now:       time.Now,
	}
}

// sign adds the Authorization header for a request. payload may be nil,
// which signs an empty body.
func (c *Client) sign(req *http.Request, payload []byte) error {
	return c.signer().sign(req, payload)
}

func (s *signer) sign(req *http.Request, payload []byte) error {
	now := s.now().UTC()
	amzDate := now.Format(isoLayout)
	dateStamp := now.Format(dateLayout)

	hash := emptyPayload
	if len(payload) > 0 {
		hash = sha256Hex(payload)
	}

	req.Header.Set("X-Amz-Date", amzDate)
	// S3 requires the payload hash header even when the body is empty; a
	// missing value is a signature mismatch rather than a clear error.
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if req.Host != "" {
		req.Header.Set("Host", req.Host)
	} else {
		req.Header.Set("Host", req.URL.Host)
	}

	canonicalRequest, signedHeaders := canonicalRequest(req, hash)
	scope := strings.Join([]string{dateStamp, s.region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := s.signingKey(dateStamp)
	signature := fmt.Sprintf("%x", hmacSHA256(signingKey, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, s.accessKey, scope, signedHeaders, signature))
	return nil
}

// signingKey derives the date/region/service-scoped key.
func (s *signer) signingKey(dateStamp string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// canonicalRequest builds the canonical form and the signed-header list.
func canonicalRequest(req *http.Request, payloadHash string) (string, string) {
	// Only headers we actually sign are listed, and they must be sorted,
	// lowercased and whitespace-trimmed to match the server's computation.
	var names []string
	for name := range req.Header {
		lower := strings.ToLower(name)
		if lower == "host" || lower == "content-type" ||
			strings.HasPrefix(lower, "x-amz-") {
			names = append(names, lower)
		}
	}
	if _, ok := req.Header["Host"]; !ok {
		names = append(names, "host")
	}
	sort.Strings(names)
	names = dedupe(names)

	var headerLines strings.Builder
	for _, name := range names {
		value := req.Header.Get(name)
		if name == "host" && value == "" {
			value = req.URL.Host
		}
		headerLines.WriteString(name)
		headerLines.WriteByte(':')
		headerLines.WriteString(strings.TrimSpace(value))
		headerLines.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalURI := canonicalPath(req.URL.Path)

	// Query parameters are sorted by key, and each key and value is
	// individually escaped.
	canonicalQuery := req.URL.Query().Encode()

	return strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		headerLines.String(),
		signedHeaders,
		payloadHash,
	}, "\n"), signedHeaders
}

// canonicalPath percent-encodes a path the way SigV4 requires: everything
// except the unreserved set and the segment separator.
//
// net/url is not usable here. EscapedPath leaves a colon literal because a
// colon is legal in a path segment, but S3 canonicalises it to %3A, so any key
// containing one -- an OCI digest, for instance -- signs differently from how
// the server reads it. PathEscape has the same gap and also escapes the
// separators.
func canonicalPath(path string) string {
	if path == "" {
		return "/"
	}
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}

const upperhex = "0123456789ABCDEF"

func dedupe(sorted []string) []string {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for _, v := range sorted[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
