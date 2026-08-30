package cli

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	withMain := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/hyprtuna/gpu-bouncer", Version: v}}
	}
	tests := []struct {
		name   string
		ldflag string
		info   *debug.BuildInfo
		want   string
	}{
		{"the release ldflag wins", "v0.1.1", withMain("v0.1.0"), "v0.1.1"},
		{"go install records the module version", "dev", withMain("v0.1.1"), "v0.1.1"},
		{"a working tree build is dev", "dev", withMain("(devel)"), "dev"},
		{"no build info at all is dev", "dev", nil, "dev"},
		{"an empty ldflag falls through to build info", "", withMain("v0.2.0"), "v0.2.0"},
		{"an empty module version is dev", "dev", withMain(""), "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.ldflag, tt.info); got != tt.want {
				t.Errorf("resolveVersion(%q, %+v) = %q, want %q", tt.ldflag, tt.info, got, tt.want)
			}
		})
	}
}

// --version is an alias for the version command: same output, exit 0.
func TestVersionFlag(t *testing.T) {
	code, stdout, stderr := run("--version")
	if code != 0 || stderr != "" {
		t.Fatalf("--version: exit %d, stderr %q", code, stderr)
	}
	_, want, _ := run("version")
	if stdout != want {
		t.Errorf("--version printed %q, version printed %q", stdout, want)
	}
	code, jsonOut, _ := run("--json", "--version")
	_, wantJSON, _ := run("--json", "version")
	if code != 0 || jsonOut != wantJSON {
		t.Errorf("--json --version printed %q (exit %d), want %q", jsonOut, code, wantJSON)
	}
}
