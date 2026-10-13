package discovery_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
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

	ownerOnly, exposure, err := fsperm.EnsureOwnerOnly(path)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !ownerOnly {
		t.Errorf("the discovery file %s", exposure)
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

// A daemon running an agent writes the file it always wrote. A client that
// predates the mode must not find a new field in the common case, and a client
// that knows it reads the absence as "running".
func TestANormalDaemonsFileSaysNothingAboutAMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	writeFile(t, path, 42)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mode") {
		t.Errorf("a normal daemon's file mentions a mode:\n%s", raw)
	}

	found, err := discovery.Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if found.Mode != "" {
		t.Errorf("mode is %q, want none", found.Mode)
	}
}

// A repair daemon says so, and has no gateway token to give: there is no
// ingress for a gateway to reach, so a token for one would only be a credential
// that reaches nothing.
func TestARepairDaemonsFileSaysSoAndCarriesNoGatewayToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := discovery.Write(path, discovery.File{
		PID:             42,
		BaseURL:         "http://127.0.0.1:1234",
		Token:           "a-token",
		ProtocolVersion: discovery.ProtocolVersion,
		Mode:            discovery.ModeRepair,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "gateway_token") {
		t.Errorf("a repair daemon's file carries a gateway token:\n%s", raw)
	}

	found, err := discovery.Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if found.Mode != discovery.ModeRepair || found.GatewayToken != "" {
		t.Errorf("read back mode %q and gateway token %q", found.Mode, found.GatewayToken)
	}
}

// The daemon is told where to put its file by JINGCLAW_SERVER_RUNTIME_DIR, and
// the commands that find it without asking it anything have to look there: a
// status that says "not running" about a daemon that is, and a stop that leaves
// it running, are what looking somewhere else was.
func TestTheDiscoveryFileIsWhereTheDaemonWasToldToPutIt(t *testing.T) {
	t.Setenv("JINGCLAW_HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", "")
	t.Setenv("JINGCLAW_RUNTIME_DIR", "")

	elsewhere := filepath.Join(t.TempDir(), "run")
	older := filepath.Join(t.TempDir(), "older")

	t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", elsewhere)
	if got, err := discovery.Path(); err != nil || got != filepath.Join(elsewhere, "daemon.json") {
		t.Errorf("the path is %q (%v), want it under %s", got, err, elsewhere)
	}

	// Both given: the one the daemon reads first.
	t.Setenv("JINGCLAW_RUNTIME_DIR", older)
	if got, _ := discovery.Path(); got != filepath.Join(elsewhere, "daemon.json") {
		t.Errorf("with both given the path is %q, want the daemon's own", got)
	}

	t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", "")
	if got, _ := discovery.Path(); got != filepath.Join(older, "daemon.json") {
		t.Errorf("with only the older variable the path is %q, want it under %s", got, older)
	}
}
