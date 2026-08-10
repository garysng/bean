package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	flags, pos := parseFlags([]string{"--image", "py:3", "--label=k=v", "sbx1", "--", "echo", "--not-a-flag"})
	if flags["image"] != "py:3" || flags["label"] != "k=v" {
		t.Errorf("flags = %v", flags)
	}
	want := []string{"sbx1", "echo", "--not-a-flag"}
	if len(pos) != 3 || pos[0] != want[0] || pos[2] != want[2] {
		t.Errorf("pos = %v, want %v", pos, want)
	}
}

func TestParseFlagsBool(t *testing.T) {
	flags, pos := parseFlags([]string{"kill-target", "--force"})
	if flags["force"] != "true" || len(pos) != 1 {
		t.Errorf("flags=%v pos=%v", flags, pos)
	}
}

func TestParseFlagsShortFlags(t *testing.T) {
	flags, pos := parseFlags([]string{"-f", "sbx1", "--label", "k=v"})
	if flags["f"] != "true" {
		t.Errorf("flags = %v, want f=true", flags)
	}
	if len(pos) != 1 || pos[0] != "sbx1" {
		t.Errorf("pos = %v", pos)
	}
	// Bundled short flags each become their own key.
	flags, _ = parseFlags([]string{"-it"})
	if flags["i"] != "true" || flags["t"] != "true" {
		t.Errorf("bundled flags = %v", flags)
	}
	// A bare "-" stays positional.
	_, pos = parseFlags([]string{"-"})
	if len(pos) != 1 || pos[0] != "-" {
		t.Errorf("pos = %v, want [-]", pos)
	}
}

func TestSplitSbxPath(t *testing.T) {
	id, path, err := splitSbxPath("sbx:sbx_abc:/workspace/a.txt")
	if err != nil || id != "sbx_abc" || path != "/workspace/a.txt" {
		t.Errorf("id=%q path=%q err=%v", id, path, err)
	}
	if _, _, err := splitSbxPath("sbx:no-colon-path"); err == nil {
		t.Error("expected error")
	}
}

func TestUsageOnUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := Run([]string{"bogus"}, &out, &errBuf)
	if code != 125 {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(errBuf.String(), "usage:") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestRunRequiresImage(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := Run([]string{"run"}, &out, &errBuf)
	if code != 125 || !strings.Contains(errBuf.String(), "--image-ref, --template or --snapshot") {
		t.Errorf("code=%d stderr=%q", code, errBuf.String())
	}
}
