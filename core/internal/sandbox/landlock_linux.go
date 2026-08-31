//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"github.com/landlock-lsm/go-landlock/landlock"
	"github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

// ConfineFlag is the first argument that means "confine, then become the
// command".
//
// The whole of the Linux backend rests on this. Landlock restricts the
// process that asks and everything it goes on to exec, and it cannot be
// undone — so the restriction has to be applied by something that is about to
// become the command and nothing else.
//
// In C that is fork, restrict, exec. In Go it cannot be: the runtime is
// multithreaded, and between fork and exec a child may touch only
// async-signal-safe things — no allocation, no locks, no scheduler. Applying
// a policy there is a game of chance with the runtime.
//
// So the daemon re-executes itself instead. The second process is a fresh
// one, single-purpose: it applies the policy and immediately replaces itself
// with the real program through execve. Nothing runs in a half-restricted
// process because there is no half-restricted process.
const ConfineFlag = "__confine"

// handlesConfinement records that this executable knows what to do when it is
// re-executed with ConfineFlag.
//
// Guarded because getting it wrong does not fail, it loops. Wrap returns this
// executable and expects it to confine and then become the command; a binary
// that does not know that runs itself again as whatever it normally is —
// which for a test binary is the tests, which start another one. It happened
// twice while this was being written, in two different packages, and both
// times it arrived as a hang rather than as an error.
//
// So a binary has to say so, and one that has not is refused rather than
// re-executed.
var handlesConfinement atomic.Bool

// WillConfine declares that this executable handles ConfineFlag.
//
// Called by main before anything else, next to the check that acts on the
// flag: the two belong together, and a binary that has one without the other
// is the failure this exists to prevent.
func WillConfine() { handlesConfinement.Store(true) }

// Confining reports whether this process was started to confine a command.
//
// Read by main before anything else: a process in this mode is not a daemon
// and must not become one.
func Confining(args []string) bool {
	return len(args) > 0 && args[0] == ConfineFlag
}

// Available reports whether commands can be confined here.
//
// Landlock is asked, rather than the kernel version. A kernel new enough can
// still have it off — it is an LSM and has to be in the boot-time list — and
// the useful question is what this machine will actually enforce.
func Available() bool {
	version, err := syscall.LandlockGetABIVersion()
	return err == nil && version >= 1
}

// abiForNetwork is the first Landlock that can refuse a connection.
//
// Before it, filesystem rules work and network rules do not exist. That gap
// is the whole of what makes Linux confinement partial, and it is why Wrap
// asks what the policy actually wanted rather than confining what it can and
// saying nothing.
const abiForNetwork = 4

// ErrNoNetworkControl says this kernel cannot refuse a connection.
var ErrNoNetworkControl = errors.New(
	"sandbox: this kernel's landlock is too old to refuse a network connection " +
		"(needs ABI 4, which is Linux 6.7); it can confine the filesystem, and a " +
		"policy asking for no network cannot be honoured")

// Wrap turns a command into the same command, confined.
//
// Returns this program, told to restrict itself and then become the command.
// The policy travels in the environment rather than on the command line for
// the same reason the Seatbelt profile is written to a file: a deployment
// with enough writable directories produces an argument longer than the
// kernel accepts.
func Wrap(policy Policy, program string, args []string) (
	string, []string, func(), error,
) {
	if !Available() {
		return "", nil, nil, ErrUnavailable
	}
	if !handlesConfinement.Load() {
		return "", nil, nil, errors.New(
			"sandbox: this program has not said it can confine anything; it would be " +
				"re-executed to apply the policy and would run itself instead " +
				"(call sandbox.WillConfine at startup, beside the check for the flag)")
	}

	// Every promise this cannot keep is refused here, before anything is
	// prepared. Refusing later — in the confined process, after it has
	// started — would arrive at the caller as a command that failed rather
	// than as confinement that was not available, and those are different
	// things to tell somebody.
	if err := keepable(policy); err != nil {
		return "", nil, nil, err
	}

	self, err := os.Executable()
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandbox: %w", err)
	}

	// Written to a file and named as an argument, the way the Seatbelt
	// profile is. The environment would have been shorter and is wrong: Wrap
	// does not own the command it is preparing, so setting a variable means
	// setting it on this whole process — and two commands confined at the
	// same time would then be reading each other's policy.
	file, err := os.CreateTemp("", "jingclaw-confine-*.json")
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	remove := func() { _ = os.Remove(file.Name()) }

	encoded, err := policy.encode()
	if err != nil {
		file.Close()
		remove()
		return "", nil, nil, err
	}
	if _, err := file.WriteString(encoded); err != nil {
		file.Close()
		remove()
		return "", nil, nil, fmt.Errorf("sandbox: write the policy: %w", err)
	}
	if err := file.Close(); err != nil {
		remove()
		return "", nil, nil, fmt.Errorf("sandbox: %w", err)
	}

	return self, append([]string{ConfineFlag, file.Name(), program}, args...), remove, nil
}

// keepable says whether this kernel can hold every part of a policy.
//
// Asked in full rather than one part at a time, because a policy is a
// statement about what will be true and half of one is not a smaller
// statement — it is a different one, which nobody made.
func keepable(policy Policy) error {
	if !policy.Network {
		version, err := syscall.LandlockGetABIVersion()
		if err != nil || version < abiForNetwork {
			return ErrNoNetworkControl
		}
	}

	// Landlock grants access rather than removing it, so a directory is
	// hidden by not being granted — which works only under a deny-by-default
	// policy, and this one deliberately is not. There is no way to express
	// "everything readable except this".
	if len(policy.Unreadable) > 0 {
		return fmt.Errorf(
			"sandbox: landlock grants access rather than removing it, so it cannot "+
				"hide %s while everything else stays readable",
			strings.Join(policy.Unreadable, ", "))
	}
	return nil
}

// Confine applies the policy in the environment and becomes the command.
//
// Never returns. Either the command is running in this process, or something
// went wrong and this exits saying so — there is no path where a command runs
// having not been confined, which is the property the whole design is for.
func Confine(args []string) {
	// args is [__confine, policy file, program, program args...].
	if len(args) < 3 {
		fail("sandbox: nothing to run")
	}

	encoded, err := os.ReadFile(args[1])
	if err != nil {
		fail("sandbox: %v", err)
	}
	policy, err := decodePolicy(string(encoded))
	if err != nil {
		fail("sandbox: %v", err)
	}

	if err := apply(policy); err != nil {
		fail("sandbox: %v", err)
	}

	args = args[2:]

	// Looked up after the policy is applied, so a program the confined
	// command could not have reached is not found on its behalf either.
	program, err := exec.LookPath(args[0])
	if err != nil {
		fail("sandbox: %v", err)
	}

	// The process becomes the command. No fork, so there is no window in
	// which a partially restricted Go runtime is doing anything.
	if err := unix.Exec(program, args, os.Environ()); err != nil {
		fail("sandbox: could not run %s: %v", args[0], err)
	}
}

// apply installs the policy, and refuses if it cannot install all of it.
func apply(policy Policy) error {
	config := landlock.V1
	if version, err := syscall.LandlockGetABIVersion(); err == nil && version >= abiForNetwork {
		config = landlock.V4
	}

	rules := []landlock.Rule{
		// Everything readable, which is where this starts. Deliberately not
		// deny-by-default: a profile that forbids everything and then permits
		// what a compiler, a package manager and a linker each need is one
		// every deployment grows by finding out what broke.
		landlock.RODirs("/"),
	}
	for _, dir := range policy.Writable {
		resolved, err := resolve(dir)
		if err != nil {
			return err
		}
		rules = append(rules, landlock.RWDirs(resolved).IgnoreIfMissing())
	}

	if !policy.Network {
		config = config.BestEffort()
		// No rule for connecting means connecting is not granted.
		return config.Restrict(rules...)
	}

	// With the network allowed, it simply is not restricted: V1 has no
	// network rules at all, and under V4 granting every port would be a
	// longer way of saying the same thing.
	return landlock.V1.Restrict(rules...)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(126) // what a shell says when a command was found and could not run
}

// encode is the policy as one environment value.
//
// JSON rather than a flag for each field: the shape is the caller's and will
// grow, and a format that has to be kept in step by hand between two halves
// of the same program is one that eventually is not.
func (p Policy) encode() (string, error) {
	encoded, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	return string(encoded), nil
}

func decodePolicy(encoded string) (Policy, error) {
	if encoded == "" {
		return Policy{}, errors.New("no policy was passed to confine with")
	}

	var policy Policy
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
		return Policy{}, fmt.Errorf("the policy is unreadable: %w", err)
	}
	return policy, nil
}

// LooksConfined reports whether a program refused because of the sandbox.
//
// Landlock refuses with EACCES, which is what a program says when a file is
// not readable for any reason at all — so this is a weaker signal than
// Seatbelt's and is named as one. What it is for is letting the observation
// the model gets name a likely cause rather than leaving it to guess why
// touch did not work.
func LooksConfined(output string) bool {
	for _, said := range []string{
		"permission denied",
		"Permission denied",
		"Operation not permitted",
		"sandbox:",
	} {
		if strings.Contains(output, said) {
			return true
		}
	}
	return false
}

// abiVersion is what this kernel's landlock offers, or zero.
func abiVersion() (int, error) {
	return syscall.LandlockGetABIVersion()
}

// describeBackend is what this kernel's landlock offers, which is not the
// same everywhere.
//
// Said in full rather than as "on". The filesystem rules and the network
// rules arrived four ABI versions apart, so a machine can confine where a
// command writes and be unable to stop it connecting — and an operator who
// asked for both should find that out at startup rather than from a policy
// that was quietly refused later.
func describeBackend() string {
	version, err := syscall.LandlockGetABIVersion()
	if err != nil {
		return "landlock (version unknown)"
	}
	if version >= abiForNetwork {
		return fmt.Sprintf("landlock ABI %d (files and network)", version)
	}
	return fmt.Sprintf(
		"landlock ABI %d (files only; refusing a connection needs ABI %d)",
		version, abiForNetwork)
}

// ForgetConfinementForTest undoes WillConfine for the rest of one test.
//
// Here rather than in a test file because the thing being checked is what a
// binary that never declared itself would get, and every test binary that
// exercises confinement has to declare itself to run at all. There is no
// other way to be in that state on purpose.
func ForgetConfinementForTest(t interface{ Cleanup(func()) }) {
	was := handlesConfinement.Swap(false)
	t.Cleanup(func() { handlesConfinement.Store(was) })
}
