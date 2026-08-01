package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

// Builds run on a node, where BuildKit and the image cache already live. The
// control plane picks a node, starts the build in the background and records the
// outcome, so a caller gets an id immediately and follows progress through the
// image endpoints — a build takes minutes, which is far longer than an HTTP
// request should be held open.

type buildRequest struct {
	Tag        string            `json:"tag"`
	Dockerfile string            `json:"dockerfile"`
	BuildArgs  map[string]string `json:"buildArgs,omitempty"`
	SizeMiB    int64             `json:"sizeMiB,omitempty"`
	// ContextTar is a base64-encoded tar of the build context, for COPY and
	// ADD. JSON has no byte type, and a multipart upload would complicate every
	// client for a field most builds leave empty.
	ContextTar string `json:"contextTar,omitempty"`
}

// maxContextBytes bounds an inline build context. A context larger than this
// wants a separate upload endpoint rather than a bigger request body.
const maxContextBytes = 64 << 20

// errNoReadyNode reports that no node can take a build.
var errNoReadyNode = errors.New("no ready node available to build on")

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}

	var req buildRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxContextBytes*2)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return
	}
	if err := image.ValidateRef(req.Tag); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if req.Dockerfile == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "dockerfile required")
		return
	}

	var contextTar []byte
	if req.ContextTar != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.ContextTar)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				"contextTar is not valid base64: "+err.Error())
			return
		}
		if len(decoded) > maxContextBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "CONTEXT_TOO_LARGE",
				"build context exceeds the inline limit")
			return
		}
		contextTar = decoded
	}

	// The tag is claimed before the build starts, both so a caller can poll and
	// so two builds cannot race to the same reference.
	if existing, err := s.store.GetImage(req.Tag); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	} else if existing != nil {
		writeErr(w, http.StatusConflict, "IMAGE_EXISTS",
			"image "+req.Tag+" already exists; images are immutable, use a new tag")
		return
	}

	img := &store.Image{
		Ref: req.Tag, Source: store.ImageBuilt, State: store.ImageBuilding,
		CreatedAt: time.Now(),
	}
	if err := s.store.PutImage(img); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	nodeID, err := s.pickBuilder()
	if err != nil {
		s.failImage(req.Tag, err.Error())
		writeErr(w, http.StatusServiceUnavailable, "NO_BUILDER", err.Error())
		return
	}

	go s.runBuild(nodeID, req, contextTar)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"imageRef": req.Tag,
		"nodeId":   nodeID,
		"state":    string(store.ImageBuilding),
	})
}

// pickBuilder chooses where to build. A node labelled as a builder is preferred,
// so a cluster can dedicate hosts to builds; otherwise any ready node will do,
// which keeps a single-node deployment working without configuration.
func (s *Server) pickBuilder() (string, error) {
	nodes, err := s.placer.Nodes()
	if err != nil {
		return "", err
	}
	var fallback string
	for _, n := range nodes {
		if n.State != string(store.NodeReady) {
			continue
		}
		if n.Labels["pool"] == "builder" {
			return n.ID, nil
		}
		if fallback == "" {
			fallback = n.ID
		}
	}
	if fallback == "" {
		return "", errNoReadyNode
	}
	return fallback, nil
}

// runBuild performs the build and records its outcome.
func (s *Server) runBuild(nodeID string, req buildRequest, contextTar []byte) {
	client, err := s.router.Client(nodeID)
	if err != nil {
		s.failImage(req.Tag, err.Error())
		return
	}

	// A build legitimately takes minutes: it pulls a base image and runs the
	// Dockerfile's commands.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	if _, err := client.BuildImage(ctx, &nodev1.BuildImageRequest{
		Tag:        req.Tag,
		Dockerfile: req.Dockerfile,
		ContextTar: contextTar,
		BuildArgs:  req.BuildArgs,
		SizeMib:    req.SizeMiB,
	}); err != nil {
		log.Printf("build %s on %s: %v", req.Tag, nodeID, err)
		s.failImage(req.Tag, err.Error())
		return
	}

	// A built image needs no conversion — BuildKit's flat output is already the
	// format the tier boots — so it goes straight to READY.
	if err := s.images.MarkReady(req.Tag, "", 0); err != nil {
		log.Printf("build %s: mark ready: %v", req.Tag, err)
	}
}
