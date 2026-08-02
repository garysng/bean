package cli

import (
	"errors"
	"fmt"
	"net/http"
)

// Exit codes. A script needs to tell "retrying will not help" from "retrying
// might", and one catch-all code cannot express that.
//
// The values follow the convention BSD documents in sysexits.h where it has an
// opinion, since that is what tooling already expects: 64 for a caller's
// mistake, 69 for a service that is not available, 70 for a failure inside the
// service. 125 stays for a usage error to match what this CLI has always
// returned for one.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitUsage means the command line was wrong. Retrying is pointless.
	ExitUsage = 125
	// ExitNotFound means the sandbox, snapshot or image does not exist.
	// Distinct from a usage error: the command was well-formed.
	ExitNotFound = 64
	// ExitUnavailable means the request never reached a verdict — the gateway
	// was unreachable, timed out, or reported no capacity. These are the
	// failures worth retrying.
	ExitUnavailable = 69
	// ExitFailed means the platform reached a verdict and it was a failure:
	// a rejected argument, a conflict, or a server-side error.
	ExitFailed = 70
)

// apiError is a response the gateway rejected, keeping the parts an exit code
// depends on. The message alone cannot say whether a retry makes sense.
type apiError struct {
	Code    string
	Message string
	Status  int
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// transportError is a request that never got an answer.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// usageError is a malformed command line found by a command rather than by the
// top-level dispatch — a missing argument, or two flags that contradict. It
// exits like any other usage error, since the distinction is where it was
// noticed, not what went wrong.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// exitCodeFor classifies a command failure.
func exitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}

	var ue *usageError
	if errors.As(err, &ue) {
		return ExitUsage
	}

	var te *transportError
	if errors.As(err, &te) {
		return ExitUnavailable
	}

	var ae *apiError
	if !errors.As(err, &ae) {
		// Something local went wrong — an unreadable context directory, a
		// malformed response. The platform is not implicated, so a retry is not
		// indicated either.
		return ExitFailed
	}
	switch ae.Status {
	case http.StatusNotFound:
		return ExitNotFound
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		http.StatusTooManyRequests:
		return ExitUnavailable
	default:
		return ExitFailed
	}
}
