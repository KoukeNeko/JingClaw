//go:build linux

package sandbox

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLandlockActuallyRefuses is the only check that matters: does the kernel
// enforce what this asked for.
//
// Run on a real Linux kernel, because everything else about a sandbox can be
// right while the thing it is for does not happen.
// TestMain makes this binary behave like the real one when it is re-executed
// to confine something.
//
// Not a convenience. Wrap returns this executable, and a test binary that did
// not know what to do with those arguments would run itself as a test again —
// which is a fork bomb, and is what happened the first time this was run.
//
// It also makes the check honest: the mechanism under test is a process
// re-executing itself, and this is a process re-executing itself.
func TestMain(m *testing.M) {
	WillConfine()

	if Confining(os.Args[1:]) {
		Confine(os.Args[1:])
	}
	os.Exit(m.Run())
}

func TestLandlockActuallyRefuses(t *testing.T) {
	if !Available() {
		t.Skip("no landlock here")
	}

	work := t.TempDir()
	writable := filepath.Join(work, "workspace")
	forbidden := filepath.Join(work, "elsewhere")
	for _, dir := range []string{writable, forbidden} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	policy := Policy{Writable: []string{writable}, Network: true}

	run := func(t *testing.T, into string) (string, error) {
		t.Helper()
		program, args, done, err := Wrap(policy, "touch", []string{filepath.Join(into, "f")})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		defer done()

		command := exec.Command(program, args...)
		out, err := command.CombinedOutput()
		return string(out), err
	}

	// The precondition. Without it, "the forbidden write failed" would pass
	// against a sandbox that refused everything.
	if out, err := run(t, writable); err != nil {
		t.Fatalf("a write to the permitted directory was refused: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(writable, "f")); err != nil {
		t.Fatalf("the permitted write did not happen: %v", err)
	}
	t.Log("ok   a write to the permitted directory happened")

	out, err := run(t, forbidden)
	if err == nil {
		t.Fatal("a write outside the permitted directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(forbidden, "f")); err == nil {
		t.Fatal("the forbidden file was created")
	}
	if !LooksConfined(out) {
		t.Errorf("the refusal does not look like one: %q", out)
	}
	t.Logf("ok   and a write outside it was refused: %s", strings.TrimSpace(out))
}

// TestAPolicyThisKernelCannotKeepIsRefused is the honesty rule.
func TestAPolicyThisKernelCannotKeepIsRefused(t *testing.T) {
	if !Available() {
		t.Skip("no landlock here")
	}

	_, _, done, err := Wrap(Policy{Network: false}, "true", nil)
	if done != nil {
		done()
	}

	version, _ := abiVersion()
	if version >= abiForNetwork {
		if err != nil {
			t.Fatalf("a kernel that can refuse a connection refused the policy: %v", err)
		}
		t.Logf("ok   ABI %d can refuse a connection, so the policy is accepted", version)
		return
	}

	if err == nil {
		t.Fatalf("ABI %d cannot refuse a connection and the policy was accepted", version)
	}
	t.Logf("ok   ABI %d cannot refuse a connection, so the policy is refused: %v", version, err)
}

// TestTheNetworkIsActuallyRefused is the other half of the policy, and the
// one it would be easiest to believe without checking.
//
// A confined command that cannot reach a listener proves nothing on its own:
// it might have been unreachable anyway. So the same address is dialled twice
// — once unconfined, to show it answers, and once confined.
func TestTheNetworkIsActuallyRefused(t *testing.T) {
	if !Available() {
		t.Skip("no landlock here")
	}
	if version, _ := abiVersion(); version < abiForNetwork {
		t.Skipf("landlock ABI %d cannot refuse a connection", version)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	address := listener.Addr().String()
	work := t.TempDir()

	dial := func(t *testing.T, network bool) (string, error) {
		t.Helper()
		program, args, done, err := Wrap(
			Policy{Writable: []string{work}, Network: network},
			"nc", []string{"-z", "-w", "2",
				strings.Split(address, ":")[0], strings.Split(address, ":")[1]})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		defer done()

		out, err := exec.Command(program, args...).CombinedOutput()
		return string(out), err
	}

	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("no nc here to dial with")
	}

	// The precondition. Without it, "the confined dial failed" would pass
	// against a listener nothing could reach.
	if out, err := dial(t, true); err != nil {
		t.Fatalf("an allowed connection was refused: %v\n%s", err, out)
	}
	t.Log("ok   with the network allowed, the listener answers")

	out, err := dial(t, false)
	if err == nil {
		t.Fatalf("a connection was made under a policy that forbade one:\n%s", out)
	}
	t.Logf("ok   and with it forbidden, the same address is refused: %s", strings.TrimSpace(out))
}
