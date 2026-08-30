package ipc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The socket must be 0660 at every instant, not only after a chmod. The
// helper process below sets umask 0, the most permissive there is, binds, and
// reports the mode bind(2) itself produced; if a window existed, that is the
// mode it would have during the window.
func TestListenCreatesSocketAt0660UnderUmaskZero(t *testing.T) {
	socket := filepath.Join(shortDir(t), "gb.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestListenHelperProcess$")
	cmd.Env = append(os.Environ(), "GPU_BOUNCER_LISTEN_HELPER="+socket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "bound mode 660\n") {
		t.Fatalf("helper output = %q, want the socket bound at mode 660", out)
	}
}

// TestListenHelperProcess is the child side of the test above. It is a no op
// unless invoked by it.
func TestListenHelperProcess(t *testing.T) {
	socket := os.Getenv("GPU_BOUNCER_LISTEN_HELPER")
	if socket == "" {
		t.Skip("helper process only")
	}
	syscall.Umask(0)
	// The mode is read through a hook placed between bind and chmod, so the
	// window itself is measured rather than the final state.
	var boundMode os.FileMode
	afterBind = func(path string) {
		if info, err := os.Stat(path); err == nil {
			boundMode = info.Mode().Perm()
		}
	}
	defer func() { afterBind = nil }()
	l, err := Listen(socket)
	if err != nil {
		fmt.Printf("listen failed: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = l.Close() }()
	fmt.Printf("bound mode %o\n", boundMode)
	info, err := os.Stat(socket)
	if err != nil {
		fmt.Printf("stat failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("final mode %o\n", info.Mode().Perm())
	// The umask must be back where the caller left it.
	if got := syscall.Umask(0); got != 0 {
		fmt.Printf("umask after Listen = %o, want 0\n", got)
		os.Exit(1)
	}
}

// The ordinary case, under the test process's own umask: the final mode is
// 0660 and the umask is restored.
func TestListenSocketMode(t *testing.T) {
	socket := filepath.Join(shortDir(t), "gb.sock")
	before := syscall.Umask(0o022)
	syscall.Umask(before)

	l, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Errorf("socket mode = %o, want 660", got)
	}
	if got := syscall.Umask(before); got != before {
		t.Errorf("umask after Listen = %o, want %o", got, before)
	}
}
