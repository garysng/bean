package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests use a stub server for the responses a healthy object store does
// not produce: server errors, malformed XML, truncated listings. They also
// pin the request shapes — URL form and query parameters — that the
// integration tests exercise but do not inspect.

func stubClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{
		Endpoint: srv.URL, Region: "us-east-1",
		AccessKey: "key", SecretKey: "secret", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewValidatesConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no endpoint":  {AccessKey: "k", SecretKey: "s"},
		"no accessKey": {Endpoint: "http://h", SecretKey: "s"},
		"no secretKey": {Endpoint: "http://h", AccessKey: "k"},
		"bad endpoint": {Endpoint: "http://[::1", AccessKey: "k", SecretKey: "s"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid config", name)
		}
	}

	// Region defaults rather than failing, since most gateways ignore it.
	c, err := New(Config{Endpoint: "http://h", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Region != "us-east-1" {
		t.Errorf("default region = %q", c.cfg.Region)
	}
}

// TestObjectURLFormsAddressStyles covers both addressing modes and the
// per-segment escaping that keeps nested keys intact.
func TestObjectURLFormsAddressStyles(t *testing.T) {
	pathStyle, err := New(Config{
		Endpoint: "http://minio:9000", AccessKey: "k", SecretKey: "s", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pathStyle.objectURL("buck", "a/b c"); got != "http://minio:9000/buck/a/b%20c" {
		t.Errorf("path-style URL = %q", got)
	}
	if got := pathStyle.bucketURL("buck"); got != "http://minio:9000/buck" {
		t.Errorf("path-style bucket URL = %q", got)
	}

	virtual, err := New(Config{
		Endpoint: "https://s3.amazonaws.com", AccessKey: "k", SecretKey: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := virtual.objectURL("buck", "a/b"); got != "https://buck.s3.amazonaws.com/a/b" {
		t.Errorf("virtual-host URL = %q", got)
	}
	if got := virtual.bucketURL("buck"); got != "https://buck.s3.amazonaws.com/" {
		t.Errorf("virtual-host bucket URL = %q", got)
	}
}

func TestGetRangeRejectsNonPositiveLength(t *testing.T) {
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not be sent for an invalid range")
	})
	for _, length := range []int64{0, -1} {
		if _, err := c.GetRange(context.Background(), "b", "k", 0, length); err == nil {
			t.Errorf("GetRange(length=%d) accepted", length)
		}
	}
}

func TestGetRangeSendsRangeHeader(t *testing.T) {
	var gotRange string
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("slice"))
	})
	r, err := c.GetRange(context.Background(), "b", "k", 10, 5)
	if err != nil {
		t.Fatalf("range get: %v", err)
	}
	body, _ := io.ReadAll(r)
	r.Close()
	if gotRange != "bytes=10-14" {
		t.Errorf("Range header = %q, want bytes=10-14", gotRange)
	}
	if string(body) != "slice" {
		t.Errorf("body = %q", body)
	}
}

// TestServerErrorsIncludeResponseBody matters for diagnosis: an S3 error body
// names the actual problem, where the status alone rarely does.
func TestServerErrorsIncludeResponseBody(t *testing.T) {
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>SignatureDoesNotMatch</Code></Error>"))
	})
	ctx := context.Background()

	err := c.PutObject(ctx, "b", "k", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("put error = %v, want the response body included", err)
	}
	if _, err := c.GetObject(ctx, "b", "k"); err == nil ||
		!strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("get error = %v", err)
	}
	if _, err := c.HeadObject(ctx, "b", "k"); err == nil {
		t.Error("head should fail on 403")
	}
	if err := c.DeleteObject(ctx, "b", "k"); err == nil {
		t.Error("delete should fail on 403")
	}
	// A non-404, non-2xx status is a real failure, not "already absent".
	if err := c.EnsureBucket(ctx, "b"); err == nil {
		t.Error("EnsureBucket should fail when creation is refused")
	}
}

func TestEnsureBucketCreatesWhenAbsent(t *testing.T) {
	var methods []string
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.EnsureBucket(context.Background(), "fresh"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if strings.Join(methods, ",") != "HEAD,PUT" {
		t.Errorf("requests = %v, want HEAD then PUT", methods)
	}
}

func TestEnsureBucketSkipsCreationWhenPresent(t *testing.T) {
	var methods []string
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusOK)
	})
	if err := c.EnsureBucket(context.Background(), "existing"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if strings.Join(methods, ",") != "HEAD" {
		t.Errorf("requests = %v, want HEAD only", methods)
	}
}

// TestEnsureBucketToleratesConcurrentCreation covers two replicas starting at
// once, where the loser sees 409 rather than success.
func TestEnsureBucketToleratesConcurrentCreation(t *testing.T) {
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusConflict)
	})
	if err := c.EnsureBucket(context.Background(), "raced"); err != nil {
		t.Errorf("ensure with concurrent creator = %v, want nil", err)
	}
}

// TestListObjectsFollowsContinuationTokens checks paging, since a truncated
// listing that silently stops would under-report during blob GC.
func TestListObjectsFollowsContinuationTokens(t *testing.T) {
	var tokens []string
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.URL.Query().Get("continuation-token"))
		if r.URL.Query().Get("list-type") != "2" {
			t.Errorf("list-type = %q, want 2", r.URL.Query().Get("list-type"))
		}
		if r.URL.Query().Get("continuation-token") == "" {
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated>`+
				`<NextContinuationToken>tok2</NextContinuationToken>`+
				`<Contents><Key>b</Key><Size>2</Size><ETag>"e2"</ETag></Contents>`+
				`</ListBucketResult>`)
			return
		}
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>a</Key><Size>1</Size><ETag>"e1"</ETag></Contents>`+
			`</ListBucketResult>`)
	})

	got, err := c.ListObjects(context.Background(), "b", "pre/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 2 || tokens[1] != "tok2" {
		t.Errorf("continuation tokens = %v", tokens)
	}
	// Results are sorted, so paging order does not leak to callers.
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "b" {
		t.Errorf("listed = %+v, want sorted a,b", got)
	}
	if got[0].Size != 1 || got[1].Size != 2 {
		t.Errorf("sizes not carried through: %+v", got)
	}
}

func TestListObjectsReportsBadResponses(t *testing.T) {
	malformed := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not xml at all")
	})
	if _, err := malformed.ListObjects(context.Background(), "b", ""); err == nil {
		t.Error("malformed XML accepted")
	}

	failing := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	if _, err := failing.ListObjects(context.Background(), "b", ""); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("list error = %v, want the body included", err)
	}
}

// TestTransportFailurePropagates covers the case where the request never
// reaches a server, which callers must distinguish from a 4xx.
func TestTransportFailurePropagates(t *testing.T) {
	c, err := New(Config{
		Endpoint: "http://127.0.0.1:1", AccessKey: "k", SecretKey: "s", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.PutObject(ctx, "b", "k", nil); err == nil {
		t.Error("put to a closed port should fail")
	}
	if _, err := c.GetObject(ctx, "b", "k"); err == nil {
		t.Error("get from a closed port should fail")
	}
	if _, err := c.HeadObject(ctx, "b", "k"); err == nil {
		t.Error("head against a closed port should fail")
	}
	if err := c.DeleteObject(ctx, "b", "k"); err == nil {
		t.Error("delete against a closed port should fail")
	}
	if _, err := c.ListObjects(ctx, "b", ""); err == nil {
		t.Error("list against a closed port should fail")
	}
	if _, err := c.NewUploader(ctx, "b", "k"); err == nil {
		t.Error("uploader against a closed port should fail")
	}
	if err := c.EnsureBucket(ctx, "b"); err == nil {
		t.Error("ensure bucket against a closed port should fail")
	}
}

func TestNotFoundIsDistinguishable(t *testing.T) {
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ctx := context.Background()
	if _, err := c.GetObject(ctx, "b", "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get = %v, want ErrNotFound", err)
	}
	if _, err := c.HeadObject(ctx, "b", "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("head = %v, want ErrNotFound", err)
	}
	if err := c.DeleteObject(ctx, "b", "k"); err != nil {
		t.Errorf("delete missing = %v, want nil", err)
	}
}
