package sandbox

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func confined(t *testing.T) (workspace string, policy Policy) {
	t.Helper()
	if !Available() {
		t.Skip("nothing on this machine can confine a command")
	}

	workspace = t.TempDir()
	return workspace, Policy{Writable: []string{workspace}}
}

// run executes a command under the policy and returns what it said.
func run(t *testing.T, policy Policy, program string, args ...string) (string, error) {
	t.Helper()

	wrapped, wrappedArgs, cleanup, err := Wrap(policy, program, args)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	defer cleanup()

	output, err := exec.Command(wrapped, wrappedArgs...).CombinedOutput()
	return string(output), err
}

// The whole point. A command may change the workspace and nothing else.
func TestItCannotWriteOutsideTheWorkspace(t *testing.T) {
	workspace, policy := confined(t)

	outside := filepath.Join(t.TempDir(), "escaped")
	output, err := run(t, policy, "/usr/bin/touch", outside)

	if err == nil {
		t.Errorf("writing outside the workspace succeeded: %s", output)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("%s was created", outside)
	}
	_ = workspace
}

// And may change the workspace, or it would confine nothing useful.
func TestItCanWriteInsideTheWorkspace(t *testing.T) {
	workspace, policy := confined(t)

	inside := filepath.Join(workspace, "allowed")
	if output, err := run(t, policy, "/usr/bin/touch", inside); err != nil {
		t.Fatalf("writing inside the workspace failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("%s was not created: %v", inside, err)
	}
}

// No network, which is the ordinary case: a build and a test each need none.
//
// Against something actually listening. A refused connection to a port
// nothing is on looks the same confined or not, so a check written that way
// passes whether or not the sandbox does anything — which is what the first
// version of this did.
func TestItCannotOpenTheNetwork(t *testing.T) {
	_, policy := confined(t)
	port := listening(t)

	// Reachable when nothing is confining it, or the refusal below would mean
	// nothing.
	if err := exec.Command("/usr/bin/nc", "-z", "-w", "1", "127.0.0.1", port).Run(); err != nil {
		t.Fatalf("the listener is not reachable unconfined, so this proves nothing: %v", err)
	}

	output, err := run(t, policy, "/usr/bin/nc", "-z", "-w", "1", "127.0.0.1", port)
	if err == nil {
		t.Errorf("a confined command reached a listening port: %s", output)
	}
}

// With it allowed, the same connection succeeds.
func TestTheNetworkCanBeAllowed(t *testing.T) {
	_, policy := confined(t)
	policy.Network = true
	port := listening(t)

	if output, err := run(t, policy, "/usr/bin/nc", "-z", "-w", "1", "127.0.0.1", port); err != nil {
		t.Errorf("the sandbox refused a connection it was told to allow: %v\n%s", err, output)
	}
}

// listening starts a TCP listener and returns the port it is on.
func listening(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return port
}

// Confining writes says nothing about reads. The places worth not showing it
// are named, and this is the one that matters most: the deployment's own
// directory is where its credentials live.
func TestItCannotReadWhatIsHidden(t *testing.T) {
	workspace, policy := confined(t)

	secrets := t.TempDir()
	secret := filepath.Join(secrets, "token")
	if err := os.WriteFile(secret, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy.Unreadable = []string{secrets}

	output, err := run(t, policy, "/bin/cat", secret)
	if err == nil {
		t.Errorf("a hidden file was read: %s", output)
	}
	if strings.Contains(output, "hunter2") {
		t.Errorf("the contents came back anyway: %s", output)
	}
	_ = workspace
}

// A machine that cannot confine says so rather than running the command.
//
// The failure this feature must never have is the quiet one: an operator who
// turned it on, a backend that is not there, and commands running exactly as
// they did before.
func TestAMachineThatCannotConfineRefuses(t *testing.T) {
	if Available() {
		t.Skip("this machine can confine, so the refusal cannot be reached here")
	}

	if _, _, _, err := Wrap(Policy{}, "/usr/bin/true", nil); err == nil {
		t.Error("wrapping succeeded on a machine with no sandbox")
	}
}

// The profile is a file because an inline one is an argument, and enough
// writable directories make an argument longer than the kernel accepts.
func TestTheProfileIsAFile(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox here")
	}

	_, args, cleanup, err := Wrap(Policy{Writable: []string{t.TempDir()}}, "/usr/bin/true", nil)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	defer cleanup()

	if len(args) < 2 || args[0] != "-f" {
		t.Fatalf("the profile was not passed as a file: %v", args)
	}
	if _, err := os.Stat(args[1]); err != nil {
		t.Errorf("the profile file is not there: %v", err)
	}

	cleanup()
	if _, err := os.Stat(args[1]); !os.IsNotExist(err) {
		t.Errorf("the profile file outlived the command")
	}
}

// A path with a quote in it is unusual and legal, and one written raw would
// end the string early and leave the rest as profile syntax.
func TestAnAwkwardPathDoesNotBreakTheProfile(t *testing.T) {
	profile, err := Profile(Policy{Writable: []string{`/tmp/it's "here"`}})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if !strings.Contains(profile, `\"here\"`) {
		t.Errorf("the quotes were not escaped:\n%s", profile)
	}
}
