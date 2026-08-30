package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

const stalenessBody = "[policy]\nvram_floor_mib = 512\npoll_interval = \"1h\"\n"

// statusStale runs status both ways and returns the text trailer and the
// config_stale value, which is a pointer because it can be unknown.
func statusStale(t *testing.T) (text string, stale *bool) {
	t.Helper()
	code, stdout, stderr := run("status")
	if code != 0 {
		t.Fatalf("status: exit %d: %s", code, stderr)
	}
	text = stdout
	_, jsonOut, _ := run("--json", "status")
	var decoded struct {
		ConfigStale *bool `json:"config_stale"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("--json status is not JSON: %v", err)
	}
	return text, decoded.ConfigStale
}

func assertNotStale(t *testing.T, what string) {
	t.Helper()
	text, stale := statusStale(t)
	if strings.Contains(text, "loaded a different config") {
		t.Errorf("%s: status calls the daemon stale:\n%s", what, text)
	}
	if stale == nil || *stale {
		t.Errorf("%s: config_stale = %v, want false", what, stale)
	}
}

// The question is whether the files the daemon loaded have changed since it
// loaded them. Everything below is a way of not changing them.
func TestStalenessIgnoresTheClientsOwnFiles(t *testing.T) {
	t.Run("untouched", func(t *testing.T) {
		startDaemonFromFile(t, stalenessBody)
		assertNotStale(t, "an untouched file")
	})

	t.Run("touched", func(t *testing.T) {
		path := startDaemonFromFile(t, stalenessBody)
		if err := os.Chtimes(path, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			t.Fatal(err)
		}
		assertNotStale(t, "a touched file")
	})

	t.Run("rewritten", func(t *testing.T) {
		path := startDaemonFromFile(t, stalenessBody)
		if err := os.WriteFile(path, []byte(stalenessBody), 0o644); err != nil {
			t.Fatal(err)
		}
		assertNotStale(t, "a byte identical rewrite")
	})

	// The cases the old comparison got wrong. The daemon's file is untouched
	// in every one of them; only what the client resolved differs.
	t.Run("other path", func(t *testing.T) {
		startDaemonFromFile(t, stalenessBody)
		other := filepath.Join(t.TempDir(), "copy.toml")
		if err := os.WriteFile(other, []byte(stalenessBody), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(config.EnvConfig, other)
		assertNotStale(t, "a client whose own file is a copy at another path")
	})

	t.Run("other xdg", func(t *testing.T) {
		startDaemonFromFile(t, stalenessBody)
		xdg := t.TempDir()
		if err := os.MkdirAll(filepath.Join(xdg, "gpu-bouncer"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(xdg, "gpu-bouncer", "config.toml"), []byte(stalenessBody), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(config.EnvConfig, "")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		assertNotStale(t, "a client on its own XDG_CONFIG_HOME")
	})

	t.Run("no file", func(t *testing.T) {
		startDaemonFromFile(t, stalenessBody)
		t.Setenv(config.EnvConfig, "")
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		assertNotStale(t, "a client that found no file")
	})
}

// A real edit is still reported, with the wording and the remedy, because a
// restart genuinely fixes it.
func TestStalenessFollowsTheDaemonsFile(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit string
	}{
		{"value", stalenessBody + "min_effect_mib = 128\n"},
		// A comment cannot change behaviour, but it does change the bytes,
		// and the digest is over bytes. Saying stale here is a false alarm
		// that costs a restart; saying nothing would need the file parsed
		// and compared semantically, which is a much bigger promise.
		{"comment", "# a note\n" + stalenessBody},
		{"whitespace", stalenessBody + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := startDaemonFromFile(t, stalenessBody)
			if err := os.WriteFile(path, []byte(tt.edit), 0o644); err != nil {
				t.Fatal(err)
			}
			text, stale := statusStale(t)
			want := "the daemon loaded a different config (" + path + ", loaded "
			if !strings.Contains(text, want) || !strings.Contains(text, "); restart it to apply your edit") {
				t.Errorf("status does not report the edit:\n%s", text)
			}
			if stale == nil || !*stale {
				t.Errorf("config_stale = %v, want true", stale)
			}
		})
	}
}

// A file the daemon loaded that cannot be read now leaves the question
// unanswered. Saying "not stale" would be a guess, and saying "stale" would
// send the operator to restart a daemon into a config that is not there.
func TestADeletedDaemonFileIsUnknown(t *testing.T) {
	path := startDaemonFromFile(t, stalenessBody)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// The client must not read that file either, or it fails before it can
	// say anything about the daemon.
	t.Setenv(config.EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	text, stale := statusStale(t)
	if stale != nil {
		t.Errorf("config_stale = %v, want null: the answer is not known", *stale)
	}
	for _, want := range []string{path, "cannot be read now", "cannot be told"} {
		if !strings.Contains(text, want) {
			t.Errorf("status does not say the daemon's file is gone (%q missing):\n%s", want, text)
		}
	}
	if strings.Contains(text, "restart it to apply your edit") {
		t.Errorf("status advises a restart for a file that is not there:\n%s", text)
	}
}

// A daemon that loaded no file at all used to be reported as having loaded a
// different config at the empty path.
func TestADaemonWithNoConfigFileSaysSo(t *testing.T) {
	// A daemon built from a config that came from no file, which is what a
	// machine with no config file anywhere produces.
	startDaemonWith(t, false)
	t.Setenv(config.EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	text, stale := statusStale(t)
	if want := "the daemon loaded no config file"; !strings.Contains(text, want) {
		t.Errorf("status does not say %q:\n%s", want, text)
	}
	if strings.Contains(text, "loaded a different config (,") {
		t.Errorf("status names an empty path:\n%s", text)
	}
	if stale == nil || *stale {
		t.Errorf("config_stale = %v, want false: a daemon with no file cannot hold an older edit", stale)
	}
}

// Without a daemon there is nothing whose configuration could be stale, so
// the answer is not known rather than false.
func TestWithoutADaemonStalenessIsUnknown(t *testing.T) {
	t.Setenv(config.EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GPU_BOUNCER_SOCKET", filepath.Join(shortDir(t), "absent.sock"))

	_, jsonOut, _ := run("--json", "status")
	var decoded struct {
		DaemonRunning bool  `json:"daemon_running"`
		ConfigStale   *bool `json:"config_stale"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if decoded.DaemonRunning {
		t.Fatal("a daemon answered, want none")
	}
	if decoded.ConfigStale != nil {
		t.Errorf("config_stale = %v, want null with no daemon", *decoded.ConfigStale)
	}
}
