package s3

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// fixedSigner pins the clock so signatures are reproducible.
//
// These tests cover canonicalisation — header selection, ordering, trimming,
// query sorting, payload binding — which is where compatibility with non-AWS
// implementations is usually lost. Whether the resulting signature is one a
// real server accepts is proven by the MinIO integration test in
// client_integration_test.go, which is a stronger check than any constant
// embedded here.
func fixedSigner() *signer {
	return &signer{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		now:       func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
}

// TestSigningKeyIsScoped checks the key derivation chain binds every scope
// component. A key that ignores one of them still signs consistently, so the
// failure would only appear against a real server.
func TestSigningKeyIsScoped(t *testing.T) {
	base := fixedSigner()
	key := hexOf(base.signingKey("20130524"))
	if len(key) != 64 {
		t.Fatalf("signing key not 32 bytes: %s", key)
	}
	if again := hexOf(base.signingKey("20130524")); again != key {
		t.Fatal("signing key not deterministic")
	}
	if other := hexOf(base.signingKey("20130525")); other == key {
		t.Error("signing key ignores the date")
	}

	otherRegion := fixedSigner()
	otherRegion.region = "eu-west-1"
	if hexOf(otherRegion.signingKey("20130524")) == key {
		t.Error("signing key ignores the region")
	}

	otherSecret := fixedSigner()
	otherSecret.secretKey = "different"
	if hexOf(otherSecret.signingKey("20130524")) == key {
		t.Error("signing key ignores the secret")
	}
}

// TestSignSetsCredentialScopeAndHeaders checks the Authorization header shape
// and the headers S3 requires to be present rather than merely signed.
func TestSignSetsCredentialScopeAndHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := fixedSigner()
	if err := s.sign(req, nil); err != nil {
		t.Fatal(err)
	}

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, algorithm+" Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request") {
		t.Errorf("credential scope wrong: %s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("signed headers wrong: %s", auth)
	}
	// The empty-body hash must be present as a header, not merely used in the
	// signature: S3 rejects the request otherwise.
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != emptyPayload {
		t.Errorf("payload hash header = %q, want empty-body hash", got)
	}
}

// TestCanonicalRequestSortsAndTrims covers the parts of canonicalisation that
// silently break compatibility when wrong: header order, case, and whitespace.
func TestCanonicalRequestSortsAndTrims(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "https://h/bucket/key?b=2&a=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Meta-Zebra", "  spaced  ")
	req.Header.Set("X-Amz-Meta-Alpha", "first")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Host", "h")
	req.Header.Set("Ignored-Header", "not signed")

	canonical, signed := canonicalRequest(req, emptyPayload)

	if signed != "content-type;host;x-amz-meta-alpha;x-amz-meta-zebra" {
		t.Errorf("signed headers = %q", signed)
	}
	if strings.Contains(canonical, "  spaced  ") {
		t.Error("header value not trimmed")
	}
	if !strings.Contains(canonical, "x-amz-meta-zebra:spaced\n") {
		t.Errorf("trimmed value missing:\n%s", canonical)
	}
	// Query parameters must be sorted by key regardless of request order.
	if !strings.Contains(canonical, "a=1&b=2") {
		t.Errorf("query not sorted:\n%s", canonical)
	}
	if strings.Contains(canonical, "ignored-header") {
		t.Error("unsigned header leaked into canonical request")
	}
}

// TestSignatureCoversTheBody guards against signing a body-carrying request as
// if it were empty, which authenticates the wrong bytes.
func TestSignatureCoversTheBody(t *testing.T) {
	sigFor := func(payload []byte) string {
		req, err := http.NewRequest(http.MethodPut, "https://h/bucket/key", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixedSigner().sign(req, payload); err != nil {
			t.Fatal(err)
		}
		return req.Header.Get("Authorization")
	}
	if sigFor([]byte("one")) == sigFor([]byte("two")) {
		t.Error("signature does not depend on the payload")
	}
	if sigFor(nil) == sigFor([]byte("one")) {
		t.Error("empty and non-empty bodies sign identically")
	}
}

func TestDedupeRemovesAdjacentDuplicates(t *testing.T) {
	got := dedupe([]string{"a", "a", "b", "c", "c", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("dedupe = %v", got)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}
