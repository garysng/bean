package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
)

// Builds run on a node, where BuildKit and the image cache already live. The
// control plane picks a node, starts the build in the background and records the
// outcome, so a caller gets an id immediately and follows progress through the
// image endpoints — a build takes minutes, which is far longer than an HTTP
// request should be held open.
//
// Progress and cancellation are two endpoints on top of that shape:
//
//   - GET  /v1/images/build/logs?ref=  streams the output
//   - POST /v1/images/build/cancel?ref=  stops the build
//
// The build is keyed by the image ref rather than by a separate build id. The
// ref is already claimed for the duration (immutable tags, one build per tag),
// so a second identifier would be a second thing to plumb through and to explain
// without describing anything the ref does not.

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

	// The build's caller owns the result. This is the case ownership exists
	// for: a caller asking "what did I build" is asking about exactly these.
	img := &store.Image{
		Ref: req.Tag, Source: store.ImageBuilt, State: store.ImageBuilding,
		Owner: s.owner(r), CreatedAt: time.Now(),
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

	// A build legitimately takes minutes: it pulls a base image and runs the
	// Dockerfile's commands. The context is detached from the request on purpose
	// — see runBuild for why a client hanging up does not stop a build.
	ctx, cancel := context.WithTimeout(context.Background(), maxBuildDuration)
	// Registered before the goroutine starts, so a caller that asks for logs the
	// instant it sees this response finds the build rather than a 404 it would
	// have to retry through.
	log := s.builds.start(req.Tag, cancel)

	go s.runBuild(ctx, cancel, nodeID, req, contextTar, log)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"imageRef": req.Tag,
		"nodeId":   nodeID,
		"state":    string(store.ImageBuilding),
	})
}

// maxBuildDuration is the ceiling on one build. It is generous because a build
// that pulls a large base image and compiles something is legitimately slow;
// what it prevents is a wedged buildctl holding a node's builder forever.
const maxBuildDuration = 60 * time.Minute

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

// runBuild performs the build, forwarding its output to log and recording the
// outcome.
//
// The context is the build's own, not any request's, and that is the decision
// worth naming: a client that hangs up does not stop the build. A build is
// expensive and its result is shared — the tag it produces is what other callers
// wait on, and its layers warm the node's BuildKit cache for everything built on
// the same base — so tearing it down because one reader closed a socket would
// throw away work nobody asked to abandon, and would make `bean build` behave
// differently depending on whether someone was watching. Stopping a build is
// therefore something a caller has to ask for explicitly, which is what the
// cancel endpoint is for.
//
// The bounded lifetime above is a different concern: it stops a wedged build
// from holding a builder indefinitely, and is not a statement about readers.
func (s *Server) runBuild(ctx context.Context, cancel context.CancelFunc,
	nodeID string, req buildRequest, contextTar []byte, log *buildLog) {

	// cancel here rather than only in the handler: releasing the context is what
	// stops the timer, and the build outliving the handler means the handler
	// cannot be the one to do it.
	defer cancel()

	fail := func(reason string) {
		log.finish(true, reason)
		s.failImage(req.Tag, reason)
	}

	client, err := s.router.Client(nodeID)
	if err != nil {
		fail(err.Error())
		return
	}

	stream, err := client.BuildImage(ctx, &nodev1.BuildImageRequest{
		Tag:        req.Tag,
		Dockerfile: req.Dockerfile,
		ContextTar: contextTar,
		BuildArgs:  req.BuildArgs,
		SizeMib:    req.SizeMiB,
	})
	if err != nil {
		slog.Error("build failed", logging.KeyImage, req.Tag, logging.KeyNode, nodeID, logging.KeyError, err)
		fail(err.Error())
		return
	}

	result, err := drainBuildStream(stream, log)
	if err != nil {
		// A cancelled build is not a failed one, but it is not a usable image
		// either: the tag has to stop claiming to be on its way, or the ref is
		// unusable until someone deletes the record by hand.
		if status.Code(err) == codes.Canceled || ctx.Err() != nil {
			slog.Info("build cancelled", logging.KeyImage, req.Tag, logging.KeyNode, nodeID)
			log.finish(true, "build cancelled")
			s.failImage(req.Tag, "build cancelled")
			return
		}
		slog.Error("build failed", logging.KeyImage, req.Tag, logging.KeyNode, nodeID, logging.KeyError, err)
		fail(status.Convert(err).Message())
		return
	}

	// A built image needs no conversion — BuildKit's flat output is sealed into an
	// overlaybd layer and published, so it goes straight to READY with the artifact's
	// real coordinates.
	//
	// The node reports an empty overlaybd_ref when it has no object store: the build
	// then exists only in the building node's ImageDir, and READY overstates its reach
	// in a multi-node cluster. Ownership is recorded regardless of where the bytes are,
	// so a later prewarm can publish it without revisiting who the image belongs to.
	if err := s.images.MarkReady(req.Tag, result.GetOverlaybdRef(), result.GetSizeBytes(),
		result.GetLayerDigests()); err != nil {
		slog.Error("cannot mark build ready", logging.KeyImage, req.Tag, logging.KeyError, err)
		fail(err.Error())
		return
	}
	log.finish(false, "")
}

// drainBuildStream copies log frames into log and returns once the node reports
// the build finished.
//
// A missing result frame is an error rather than a success: the node sends it
// last, so a stream that ends without one means the build did not get to the end
// and marking the image READY would publish a tag with no image behind it.
func drainBuildStream(stream nodev1.SandboxService_BuildImageClient, log *buildLog) (*nodev1.BuildImageResponse, error) {
	var result *nodev1.BuildImageResponse
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			if result == nil {
				return nil, errors.New("node ended the build stream without a result")
			}
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if data := ev.GetLog(); len(data) > 0 {
			_, _ = log.Write(data)
		}
		if r := ev.GetResult(); r != nil {
			result = r
		}
	}
}

// handleBuildLogs streams a build's output as chunked plain text.
//
// Chunked text rather than SSE or a long-poll cursor. The payload is a log: it
// is already a byte stream, and SSE would mean framing every line as an event
// and unframing it in each client for no gain — /v1/events uses SSE because its
// payload is discrete typed JSON objects, which is the opposite case. A cursor
// endpoint would put reassembly in the client and turn one connection into a
// polling loop. Plain chunked text is what `curl` and `bean build --follow` can
// both consume without a parser.
//
// ?follow=false returns what has been produced so far and stops, which is what a
// script wants after a build has finished.
func (s *Server) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "ref query param required")
		return
	}
	log := s.builds.get(ref)
	if log == nil {
		writeErr(w, http.StatusNotFound, "BUILD_NOT_FOUND",
			"no build logs retained for "+ref)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported")
		return
	}

	follow := r.URL.Query().Get("follow") != "false"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Buffering a log stream defeats it, and a proxy deciding to do so is the
	// usual cause of "no output until the build ends".
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	var offset int64
	for {
		data, at, done := log.read(offset)
		if at > offset && offset > 0 {
			// The window slid past what this reader had. Saying so beats a silent
			// gap, which would read as a build that skipped a step.
			_, _ = io.WriteString(w, "\n[log truncated: earlier output dropped]\n")
		}
		offset = at + int64(len(data))
		if len(data) > 0 {
			if _, err := w.Write(data); err != nil {
				return
			}
			flusher.Flush()
		}
		if done || !follow {
			break
		}
		if !log.wait(r.Context(), offset) {
			// Reader hung up. The build keeps going; see runBuild.
			return
		}
	}

	// The outcome goes in the body rather than the status: the response was
	// committed with 200 before the build's fate was known, so a reader that only
	// checked the status would call a failed build a success.
	if done, failed, reason := log.status(); done {
		if failed {
			_, _ = fmt.Fprintf(w, "\nbuild failed: %s\n", reason)
		} else {
			_, _ = io.WriteString(w, "\nbuild succeeded\n")
		}
		flusher.Flush()
	}
}

// handleBuildCancel stops a running build.
//
// Cancelling is explicit because a build's result is shared: a reader
// disconnecting does not imply nobody wants the image (see runBuild). The
// cancelled build's tag is marked FAILED, which frees the ref for another
// attempt.
func (s *Server) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "ref query param required")
		return
	}
	log := s.builds.get(ref)
	if log == nil {
		writeErr(w, http.StatusNotFound, "BUILD_NOT_FOUND", "no build in progress for "+ref)
		return
	}
	if done, _, _ := log.status(); done {
		// Already over: nothing to stop, and reporting success would suggest this
		// call changed something.
		writeErr(w, http.StatusConflict, "BUILD_FINISHED",
			"build for "+ref+" has already finished")
		return
	}
	// Cancelling the context kills buildctl on the node. runBuild observes the
	// cancellation and settles the image record, so this handler does not touch
	// it — two writers to that state is how it ends up disagreeing.
	log.cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"imageRef": ref,
		"state":    "CANCELLING",
	})
}
