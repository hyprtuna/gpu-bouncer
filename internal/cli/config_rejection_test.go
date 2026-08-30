package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// A config a file cannot legally hold must stop the command, with the reason
// on stderr and exit 1. A value silently replaced by a default is how an
// operator ends up running a daemon they did not configure.
func TestARefusedConfigExitsOneAndSaysWhy(t *testing.T) {
	bodies := map[string]string{
		"bare zero poll_interval":   "[policy]\npoll_interval = 0\n",
		"bare zero action_cooldown": "[policy]\naction_cooldown = -0\n",
		"bare zero timeout":         "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\ntimeout = 0\n",
		"bare zero drain_timeout":   "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\ndrain_timeout = 0\n",
		"negative vram_floor_mib":   "[policy]\nvram_floor_mib = -1\n",
		"poll_interval under 1s":    "[policy]\npoll_interval = \"999ms\"\n",
		"drain_timeout over 10m":    "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\ndrain_timeout = \"11m\"\n",
	}
	for name, body := range bodies {
		for _, command := range []string{"status", "plan"} {
			t.Run(name+"/"+command, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "c.toml")
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				// No daemon: these two commands read the config themselves,
				// which is where the refusal has to happen.
				t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "absent.sock"))

				code, stdout, stderr := run("--config", path, command)
				if code != 1 {
					t.Errorf("exit = %d, want 1 (stdout %q, stderr %q)", code, stdout, stderr)
				}
				if !strings.Contains(stderr, "c.toml") {
					t.Errorf("stderr = %q, want it to name the file", stderr)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want it empty", stdout)
				}

				// The environment variable is the same code path and must
				// give the same answer.
				t.Setenv(config.EnvConfig, path)
				envCode, _, envErr := run(command)
				if envCode != code || envErr != stderr {
					t.Errorf("GPU_BOUNCER_CONFIG gave (%d, %q), want the same as --config (%d, %q)",
						envCode, envErr, code, stderr)
				}
			})
		}
	}
}

// With --json the refusal is still one JSON object on stdout and exit 1, so a
// script never has to parse stderr.
func TestARefusedConfigInJSONIsStillOneObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte("[policy]\npoll_interval = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "absent.sock"))

	code, stdout, stderr := run("--json", "--config", path, "status")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty", stderr)
	}
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout %q is not one JSON object: %v", stdout, err)
	}
	if body.OK {
		t.Errorf("ok = true on a refused config")
	}
	if !strings.Contains(body.Error, `must be a duration string such as "5s"`) {
		t.Errorf("error = %q, want it to say what the key takes", body.Error)
	}
}
