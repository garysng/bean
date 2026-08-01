package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseReferenceCoversTheUsualForms pins the Docker-inherited rules, which
// are surprising enough that getting them wrong is easy: whether a leading
// component is a registry host depends on whether it looks like one.
func TestParseReferenceCoversTheUsualForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Reference
	}{
		{"alpine", Reference{Host: "registry-1.docker.io", Repository: "library/alpine", Tag: "latest"}},
		{"alpine:3.20", Reference{Host: "registry-1.docker.io", Repository: "library/alpine", Tag: "3.20"}},
		// A slash without a dot or colon is a Hub namespace, not a host.
		{"bitnami/minio", Reference{Host: "registry-1.docker.io", Repository: "bitnami/minio", Tag: "latest"}},
		{"ghcr.io/owner/img:v1", Reference{Host: "ghcr.io", Repository: "owner/img", Tag: "v1"}},
		// A colon in the first component is a port, not a tag.
		{"localhost:5000/img:v2", Reference{Host: "localhost:5000", Repository: "img", Tag: "v2"}},
		{"localhost/img", Reference{Host: "localhost", Repository: "img", Tag: "latest"}},
		{
			"alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Reference{
				Host: "registry-1.docker.io", Repository: "library/alpine",
				Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
		},
	} {
		got, err := ParseReference(tc.in)
		if err != nil {
			t.Errorf("ParseReference(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseReference(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseReferenceRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"alpine:",
		"alpine@sha256:short",
		"alpine@md5:0000",
		"ghcr.io/",
	} {
		if ref, err := ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) accepted, got %+v", in, ref)
		}
	}
}

// TestParseChallengeHandlesCommasInValues covers the case a naive split breaks:
// a scope value contains commas when it names more than one repository.
func TestParseChallengeHandlesCommasInValues(t *testing.T) {
	header := `Bearer realm="https://auth.example.com/token",service="registry",` +
		`scope="repository:a/b:pull,repository:c/d:pull"`
	got := parseChallenge(header)

	if got["realm"] != "https://auth.example.com/token" {
		t.Errorf("realm = %q", got["realm"])
	}
	if got["service"] != "registry" {
		t.Errorf("service = %q", got["service"])
	}
	if got["scope"] != "repository:a/b:pull,repository:c/d:pull" {
		t.Errorf("scope = %q, want both repositories intact", got["scope"])
	}
}

// fakeRegistry serves the token challenge, a manifest and a config blob, so the
// pull path is tested against the protocol rather than a mock of itself.
type fakeRegistry struct {
	manifest    []byte
	manifestCT  string
	platform    []byte
	config      []byte
	layer       []byte
	tokenIssued int
	authHeader  string
}

func (f *fakeRegistry) serve(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenIssued++
		f.authHeader = r.Header.Get("Authorization")
		if r.URL.Query().Get("scope") == "" {
			t.Error("token request carried no scope")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token": "test-token", "expires_in": 300,
		})
	})

	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		// Every registry request must be authenticated, which is what drives
		// the challenge-and-retry path.
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token",service="fake"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			// The platform-specific manifest is served by digest, the index by tag.
			if strings.Contains(r.URL.Path, "sha256:platform") {
				w.Header().Set("Content-Type", mediaTypeManifestV2)
				w.Write(f.platform)
				return
			}
			w.Header().Set("Content-Type", f.manifestCT)
			w.Write(f.manifest)
		case strings.Contains(r.URL.Path, "/blobs/"):
			if strings.Contains(r.URL.Path, "config") {
				w.Write(f.config)
				return
			}
			w.Write(f.layer)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// hostRef points a Reference at the test server, which speaks plain HTTP.
func hostRef(t *testing.T, srv *httptest.Server) (Reference, *Registry) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	reg := NewRegistry(nil)
	// The client is redirected to http, since httptest does not serve TLS here.
	reg.Client = srv.Client()
	reg.Client.Transport = rewriteToHTTP{base: srv.Client().Transport}
	return Reference{Host: host, Repository: "test/img", Tag: "latest"}, reg
}

// rewriteToHTTP downgrades the scheme so the production code's https URLs reach
// the test server unchanged otherwise.
type rewriteToHTTP struct{ base http.RoundTripper }

func (rt rewriteToHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		req.URL.Scheme = "http"
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func TestFetchManifestAuthenticatesAndCachesToken(t *testing.T) {
	fake := &fakeRegistry{
		manifestCT: mediaTypeManifestV2,
		manifest: []byte(`{"config":{"digest":"sha256:config","size":10},` +
			`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",` +
			`"digest":"sha256:layer1","size":100}]}`),
		config: []byte(`{"config":{"Env":["PATH=/usr/bin"],"Cmd":["/bin/sh"]}}`),
	}
	srv := fake.serve(t)
	ref, reg := hostRef(t, srv)

	m, err := reg.FetchManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if len(m.Layers) != 1 || m.Layers[0].Digest != "sha256:layer1" {
		t.Errorf("layers = %+v", m.Layers)
	}
	if m.Digest == "" {
		t.Error("manifest digest not set")
	}

	// A second call must reuse the token: a pull makes one request per layer,
	// and re-authenticating each time would triple the round trips.
	before := fake.tokenIssued
	if _, err := reg.FetchManifest(context.Background(), ref); err != nil {
		t.Fatalf("second FetchManifest: %v", err)
	}
	if fake.tokenIssued != before {
		t.Errorf("token fetched again (%d then %d); it should be cached",
			before, fake.tokenIssued)
	}
}

// TestFetchManifestResolvesPlatformIndex covers multi-platform images, where the
// tag points at an index rather than a manifest.
func TestFetchManifestResolvesPlatformIndex(t *testing.T) {
	fake := &fakeRegistry{
		manifestCT: mediaTypeOCIIndex,
		manifest: []byte(`{"manifests":[
			{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},
			{"digest":"sha256:platform","platform":{"os":"linux","architecture":"amd64"}}
		]}`),
		platform: []byte(`{"config":{"digest":"sha256:config","size":10},` +
			`"layers":[{"digest":"sha256:amd64layer","size":50}]}`),
	}
	srv := fake.serve(t)
	ref, reg := hostRef(t, srv)

	m, err := reg.FetchManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if len(m.Layers) != 1 || m.Layers[0].Digest != "sha256:amd64layer" {
		t.Errorf("resolved to the wrong platform: %+v", m.Layers)
	}
}

// TestFetchManifestRejectsIndexWithoutAmd64 matters because the guest kernel is
// x86-64: another architecture's layers would build a rootfs whose binaries
// cannot run, which is worth failing early rather than at boot.
func TestFetchManifestRejectsIndexWithoutAmd64(t *testing.T) {
	fake := &fakeRegistry{
		manifestCT: mediaTypeOCIIndex,
		manifest: []byte(`{"manifests":[
			{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}}
		]}`),
	}
	srv := fake.serve(t)
	ref, reg := hostRef(t, srv)

	_, err := reg.FetchManifest(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("err = %v, want it to name the missing architecture", err)
	}
}

func TestFetchManifestRejectsEmptyLayers(t *testing.T) {
	fake := &fakeRegistry{
		manifestCT: mediaTypeManifestV2,
		manifest:   []byte(`{"config":{"digest":"sha256:config"},"layers":[]}`),
	}
	srv := fake.serve(t)
	ref, reg := hostRef(t, srv)

	if _, err := reg.FetchManifest(context.Background(), ref); err == nil {
		t.Error("accepted a manifest with no layers")
	}
}

func TestFetchConfigReadsImageSettings(t *testing.T) {
	fake := &fakeRegistry{
		manifestCT: mediaTypeManifestV2,
		manifest: []byte(`{"config":{"digest":"sha256:config","size":10},` +
			`"layers":[{"digest":"sha256:l1","size":10}]}`),
		config: []byte(`{"config":{"Env":["A=b"],"Cmd":["/bin/sh"],"WorkingDir":"/w"}}`),
	}
	srv := fake.serve(t)
	ref, reg := hostRef(t, srv)

	m, err := reg.FetchManifest(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := reg.FetchConfig(context.Background(), ref, m)
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if len(cfg.Env) != 1 || cfg.Env[0] != "A=b" {
		t.Errorf("env = %v", cfg.Env)
	}
	if cfg.WorkingDir != "/w" {
		t.Errorf("workingDir = %q", cfg.WorkingDir)
	}
}

// TestCredentialSourceIsUsed checks a private registry's credentials reach the
// token exchange rather than being silently dropped.
func TestCredentialSourceIsUsed(t *testing.T) {
	fake := &fakeRegistry{
		manifestCT: mediaTypeManifestV2,
		manifest: []byte(`{"config":{"digest":"sha256:config"},` +
			`"layers":[{"digest":"sha256:l1","size":1}]}`),
	}
	srv := fake.serve(t)
	ref, reg := hostRef(t, srv)
	reg.Auth = staticCreds{host: ref.Host, user: "alice", pass: "secret"}

	if _, err := reg.FetchManifest(context.Background(), ref); err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if !strings.HasPrefix(fake.authHeader, "Basic ") {
		t.Errorf("token request auth = %q, want basic credentials", fake.authHeader)
	}
}

type staticCreds struct {
	host, user, pass string
}

func (s staticCreds) Credential(host string) (string, string, bool) {
	if host != s.host {
		return "", "", false
	}
	return s.user, s.pass, true
}

func TestFetchBlobReportsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "no such blob")
	}))
	t.Cleanup(srv.Close)

	ref, reg := hostRef(t, srv)
	_, err := reg.FetchBlob(context.Background(), ref, "sha256:absent")
	if err == nil {
		t.Fatal("FetchBlob accepted a 404")
	}
	if !strings.Contains(err.Error(), "no such blob") {
		t.Errorf("err = %v, want the response body included", err)
	}
}
