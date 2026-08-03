package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/secret"
	"github.com/garysng/bean/internal/control/store"
)

// Registry credential endpoints. Callers register a credential per registry
// host once; afterwards a private image is referenced by nothing but its
// ref, exactly like a public one.
//
// The secret is write-only over the API: it is encrypted before it reaches
// the database and never appears in a response, a log line, or a sandbox.

type registryRequest struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Secret   string `json:"secret"`
}

func (s *Server) handlePutRegistry(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED",
			"registry credentials require a master key (--secret-key or BEAN_SECRET_KEY)")
		return
	}
	var req registryRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return
	}
	host := normalizeRegistryHost(req.Host)
	if host == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "host is required")
		return
	}
	if req.Secret == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "secret is required")
		return
	}

	ciphertext, err := s.secrets.Seal(req.Secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "seal secret: "+err.Error())
		return
	}
	cred := &store.RegistryCredential{
		Host:             host,
		Username:         req.Username,
		SecretCiphertext: ciphertext,
		CreatedAt:        time.Now(),
	}
	if err := s.store.PutRegistryCredential(cred); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	// Response carries no secret material.
	writeJSON(w, http.StatusOK, map[string]any{
		"host": cred.Host, "username": cred.Username, "updatedAt": cred.UpdatedAt,
	})
}

func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	creds, err := s.store.ListRegistryCredentials()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		out = append(out, map[string]any{
			"host": c.Host, "username": c.Username,
			"createdAt": c.CreatedAt, "updatedAt": c.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"registries": out})
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	host, err := url.PathUnescape(r.PathValue("host"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "malformed host encoding")
		return
	}
	switch err := s.store.DeleteRegistryCredential(normalizeRegistryHost(host)); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "REGISTRY_NOT_FOUND", "no credential for "+host)
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// normalizeRegistryHost strips a scheme and trailing slash so that
// "https://reg.example.com/" and "reg.example.com" are the same key.
func normalizeRegistryHost(host string) string {
	h := strings.TrimSpace(host)
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimSuffix(h, "/")
	return h
}

// RegistryAuth resolves the credential for an image reference, decrypting
// the secret. It is how the image pipeline (and later, per-node token
// minting) obtains authentication. Returns nil when the registry needs no
// credential, which is the common case for public images.
func (s *Server) RegistryAuth(imageRef string) (username, password string, err error) {
	host := RegistryHostOf(imageRef)
	cred, err := s.store.GetRegistryCredential(host)
	if err != nil || cred == nil {
		return "", "", err
	}
	if s.secrets == nil {
		return "", "", secret.ErrNoKey
	}
	plaintext, err := s.secrets.Open(cred.SecretCiphertext)
	if err != nil {
		return "", "", err
	}
	return cred.Username, plaintext, nil
}

// RegistryHostOf extracts the registry host from an OCI reference. It
// delegates to the image package so credential lookup and the policy
// allowlist cannot disagree about which host a reference names — two copies of
// this rule would mean a ref whose credential is found but whose registry is
// refused, or the reverse.
func RegistryHostOf(ref string) string {
	return image.RegistryHost(ref)
}
