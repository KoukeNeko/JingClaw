package discovery_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

func writeFile(t *testing.T, path string, pid int) {
	t.Helper()

	if err := discovery.Write(path, discovery.File{
		PID:             pid,
		BaseURL:         "http://127.0.0.1:1234",
		Token:           "a-token",
		ProtocolVersion: discovery.ProtocolVersion,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// It holds the control token, so it must never be readable by anybody else on
// a shared machine.
func TestTheDiscoveryFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "daemon.json")
	writeFile(t, path, 42)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %o, want 600", perm)
	}
}

func TestADaemonCleansUpAfterItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	writeFile(t, path, os.Getpid())

	if err := discovery.RemoveIfOwnedBy(path, os.Getpid()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the file is still there")
	}
}

// A daemon that has already been replaced must not delete its replacement's
// file on the way out. Getting this wrong leaves a running daemon that no
// client can find, which from the outside looks exactly like it is down.
func TestADaemonDoesNotDeleteItsReplacementsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	writeFile(t, path, 9999)

	if err := discovery.RemoveIfOwnedBy(path, 1234); err != nil {
		t.Fatalf("remove: %v", err)
	}

	found, err := discovery.Read(path)
	if err != nil {
		t.Fatalf("the replacement's file was deleted: %v", err)
	}
	if found.PID != 9999 {
		t.Errorf("pid is %d, want the replacement's 9999", found.PID)
	}
}

func TestRemovingSomethingAlreadyGoneIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")

	if err := discovery.RemoveIfOwnedBy(path, os.Getpid()); err != nil {
		t.Errorf("removing a file that is not there failed: %v", err)
	}
}
