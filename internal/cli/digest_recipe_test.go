package cli

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// serveDaemonReporting answers ping and status with a daemon_config the test
// dictates, so the client's half of a mixed-version pair can be exercised
// without building the other version's binary.
func serveDaemonReporting(t *testing.T, report ipc.ConfigReport) {
	t.Helper()
	socket := filepath.Join(shortDir(t), "stub.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	dry := false
	body, err := json.Marshal(ipc.Response{
		OK:            true,
		Message:       "gpu-bouncer daemon is running",
		DaemonDryRun:  &dry,
		DaemonConfig:  &report,
		DaemonRunning: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
					return
				}
				_, _ = conn.Write(append(body, '\n'))
			}()
		}
	}()
	t.Setenv(ipc.EnvSocket, socket)
}

// reportFor is a daemon's account of a config file that exists and has not
// been edited: the real digest of the real file, under the named recipe.
func reportFor(t *testing.T, recipe string) ipc.ConfigReport {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte(stalenessBody), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfig, path)
	digest, err := config.ContentDigest([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return ipc.ConfigReport{Path: path, Paths: []string{path}, SHA256: digest, DigestRecipe: recipe}
}

// A daemon older than the field sends a digest it computed some other way.
// Comparing it with this client's recipe can only ever mismatch, which is
// what reported an untouched file as edited on every status of a mixed pair.
func TestADigestWithNoRecipeIsNotCompared(t *testing.T) {
	report := reportFor(t, "")
	// The digest a 0.1.2 daemon would have sent: the path folded in too.
	report.SHA256 = "add2afd374d40802092246772868f7e9edc727238cfda280b686738f3c90ce87"
	serveDaemonReporting(t, report)

	text, stale := statusStale(t)
	if stale != nil {
		t.Errorf("config_stale = %v, want null: the two digests are not comparable", *stale)
	}
	if !strings.Contains(text, "older than this client") ||
		!strings.Contains(text, "whether it is running on an older edit cannot be told") {
		t.Errorf("status does not say the answer is unknown:\n%s", text)
	}
	if strings.Contains(text, "restart it to apply your edit") {
		t.Errorf("status tells the operator to restart a daemon over a file nobody edited:\n%s", text)
	}
}

// A recipe this client does not implement is the same situation, from the
// other direction: a daemon newer than the client, or one built otherwise.
func TestAnUnknownDigestRecipeIsNotCompared(t *testing.T) {
	serveDaemonReporting(t, reportFor(t, "path-and-content-v0"))

	text, stale := statusStale(t)
	if stale != nil {
		t.Errorf("config_stale = %v, want null on an unknown recipe", *stale)
	}
	if !strings.Contains(text, `"path-and-content-v0"`) ||
		!strings.Contains(text, "whether it is running on an older edit cannot be told") {
		t.Errorf("status does not name the recipe it cannot reproduce:\n%s", text)
	}
	if strings.Contains(text, "restart it to apply your edit") {
		t.Errorf("status calls an unreadable comparison an edit:\n%s", text)
	}
}

// The recipe this client does implement still compares, both ways round.
func TestTheCurrentRecipeStillCompares(t *testing.T) {
	report := reportFor(t, config.DigestRecipe)
	serveDaemonReporting(t, report)
	text, stale := statusStale(t)
	if stale == nil || *stale {
		t.Errorf("config_stale = %v, want false on an untouched file:\n%s", stale, text)
	}

	// The same daemon, after the file it loaded has been edited.
	edited := report
	edited.SHA256 = strings.Repeat("0", 64)
	serveDaemonReporting(t, edited)
	text, stale = statusStale(t)
	if stale == nil || !*stale {
		t.Errorf("config_stale = %v, want true after an edit:\n%s", stale, text)
	}
	if !strings.Contains(text, "restart it to apply your edit") {
		t.Errorf("an edited file does not print the remedy:\n%s", text)
	}
}

// A daemon of this release says which recipe it used, so the next client to
// change the recipe can tell that it must not compare.
func TestTheDaemonReportsItsDigestRecipe(t *testing.T) {
	startDaemonFromFile(t, stalenessBody)
	_, jsonOut, _ := run("--json", "status")
	var decoded struct {
		DaemonConfig *ipc.ConfigReport `json:"daemon_config"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("--json status is not JSON: %v", err)
	}
	if decoded.DaemonConfig == nil {
		t.Fatal("daemon_config is null")
	}
	if got := decoded.DaemonConfig.DigestRecipe; got != config.DigestRecipe {
		t.Errorf("digest_recipe = %q, want %q", got, config.DigestRecipe)
	}
	if !strings.Contains(jsonOut, `"digest_recipe"`) {
		t.Errorf("the key is absent from --json status:\n%s", jsonOut)
	}
}
