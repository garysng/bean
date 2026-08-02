package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestExitCodeDistinguishesRetryableFailures(t *testing.T) {
	// A script's only question is whether to retry, so these must not collapse
	// into one code.
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, ExitOK},
		{"usage", usagef("usage: bean run --image IMG"), ExitUsage},
		{"unreachable gateway", &transportError{err: errors.New("connection refused")},
			ExitUnavailable},
		{"missing sandbox", &apiError{Code: "SANDBOX_NOT_FOUND", Status: http.StatusNotFound},
			ExitNotFound},
		{"no capacity", &apiError{Code: "NO_CAPACITY", Status: http.StatusServiceUnavailable},
			ExitUnavailable},
		{"rate limited", &apiError{Status: http.StatusTooManyRequests}, ExitUnavailable},
		{"rejected argument", &apiError{Code: "IMAGE_REF_INVALID", Status: http.StatusBadRequest},
			ExitFailed},
		{"server error", &apiError{Status: http.StatusInternalServerError}, ExitFailed},
		{"local failure", errors.New("cannot read build context"), ExitFailed},
	} {
		if got := exitCodeFor(tc.err); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestApiErrorMessageIncludesTheCode(t *testing.T) {
	e := &apiError{Code: "SANDBOX_NOT_FOUND", Message: "no such sandbox", Status: 404}
	if got := e.Error(); !strings.Contains(got, "SANDBOX_NOT_FOUND") {
		t.Errorf("Error() = %q, want the code in it", got)
	}
	// Without a code there is still a status worth showing, or the message reads
	// as though nothing went wrong.
	bare := &apiError{Message: "upstream exploded", Status: 502}
	if got := bare.Error(); !strings.Contains(got, "502") {
		t.Errorf("Error() = %q, want the status in it", got)
	}
}

func TestPrinterTableRendersColumnsForAPerson(t *testing.T) {
	var buf bytes.Buffer
	p := newPrinter(&buf, nil)
	err := p.table("sandboxes", []row{
		newRow("id", "sbx_1").with("state", "RUNNING").with("sizeBytes", 42),
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The heading comes from the field names, so a new field cannot appear in
	// the rows without appearing in the heading.
	if !strings.Contains(out, "ID") || !strings.Contains(out, "STATE") {
		t.Errorf("no heading in %q", out)
	}
	// camelCase becomes separate words rather than running together.
	if !strings.Contains(out, "SIZE BYTES") {
		t.Errorf("sizeBytes heading = %q", out)
	}
	if !strings.Contains(out, "sbx_1") {
		t.Errorf("missing the row: %q", out)
	}
}

func TestPrinterTableJSONUsesFieldNamesAsKeys(t *testing.T) {
	var buf bytes.Buffer
	p := newPrinter(&buf, map[string]string{"json": "true"})
	if err := p.table("sandboxes", []row{
		newRow("id", "sbx_1").with("state", "RUNNING"),
		newRow("id", "sbx_2").with("state", "PAUSED"),
	}); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got.Sandboxes) != 2 {
		t.Fatalf("got %d items: %s", len(got.Sandboxes), buf.String())
	}
	if got.Sandboxes[0]["id"] != "sbx_1" || got.Sandboxes[1]["state"] != "PAUSED" {
		t.Errorf("decoded = %+v", got.Sandboxes)
	}
}

// TestPrinterEmptyTableIsValidJSON covers a listing with no results. A script
// decoding the output must not have to handle "no output at all" as a case.
func TestPrinterEmptyTableIsValidJSON(t *testing.T) {
	var buf bytes.Buffer
	p := newPrinter(&buf, map[string]string{"json": "true"})
	if err := p.table("sandboxes", nil); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("empty listing is not valid JSON: %v (%q)", err, buf.String())
	}
	if _, ok := got["sandboxes"]; !ok {
		t.Errorf("empty listing = %q, want the key present", buf.String())
	}
}

func TestPrinterQuietPrintsOnlyIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	p := newPrinter(&buf, map[string]string{"quiet": "true"})
	if err := p.table("sandboxes", []row{
		newRow("id", "sbx_1").with("state", "RUNNING"),
		newRow("id", "sbx_2").with("state", "PAUSED"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "sbx_1\nsbx_2\n" {
		t.Errorf("quiet output = %q", got)
	}
}

// TestPrinterNoteIsForPeopleOnly guards the property that makes --json usable:
// progress chatter mixed into the stream would break every decoder.
func TestPrinterNoteIsForPeopleOnly(t *testing.T) {
	for _, mode := range []string{"json", "quiet"} {
		var buf bytes.Buffer
		p := newPrinter(&buf, map[string]string{mode: "true"})
		p.note("uploading %d KiB", 12)
		if buf.Len() != 0 {
			t.Errorf("--%s emitted a note: %q", mode, buf.String())
		}
	}

	var buf bytes.Buffer
	newPrinter(&buf, nil).note("uploading %d KiB", 12)
	if !strings.Contains(buf.String(), "uploading 12 KiB") {
		t.Errorf("plain output dropped the note: %q", buf.String())
	}
}

func TestPrinterResultKeepsOneLineShape(t *testing.T) {
	var buf bytes.Buffer
	if err := newPrinter(&buf, nil).result("sbx_1",
		field{"state", "RUNNING"}); err != nil {
		t.Fatal(err)
	}
	// Scripts already cut this line on tabs, so the id stays first and the line
	// stays single.
	if got := buf.String(); got != "sbx_1\tRUNNING\n" {
		t.Errorf("result = %q", got)
	}
}

func TestSortedLabelsIsDeterministic(t *testing.T) {
	labels := map[string]string{"z": "1", "a": "2", "m": "3"}
	// Map iteration order varies per run, so an unsorted rendering would make
	// output differ between identical invocations.
	first := sortedLabels(labels)
	for i := 0; i < 20; i++ {
		if got := sortedLabels(labels); got != first {
			t.Fatalf("run %d = %q, first = %q", i, got, first)
		}
	}
	if first != "a=2,m=3,z=1" {
		t.Errorf("sortedLabels = %q", first)
	}
	if sortedLabels(nil) != "" {
		t.Error("nil labels should render as empty")
	}
}

func TestLabelsEncodeAsAnObjectNotNull(t *testing.T) {
	// A caller indexing .labels must not have to check for null first.
	b, err := json.Marshal(orEmptyMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Errorf("nil labels encoded as %s, want {}", b)
	}
}
