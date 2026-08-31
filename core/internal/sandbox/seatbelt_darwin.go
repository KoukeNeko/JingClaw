//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// seatbelt is the program that applies a profile. Undocumented and not a
// supported interface, which is recorded where somebody will read it: see the
// note on Available.
const seatbelt = "/usr/bin/sandbox-exec"

// SeatbeltEnv names the program to use instead, for a check that has to be a
// machine where confinement is unavailable.
//
// That case is the one this feature turns on and the one no ordinary machine
// can reach: every Mac has sandbox-exec, so "what happens when it is missing"
// is unreachable without saying where to look. Read only here, and never
// consulted for anything but the path.
const SeatbeltEnv = "JINGCLAW_SANDBOX_EXEC"

// applier is the program that will be run.
func applier() string {
	if named := os.Getenv(SeatbeltEnv); named != "" {
		return named
	}
	return seatbelt
}

// Available reports whether commands can be confined here.
//
// macOS only, for now, and through an interface Apple does not support: the
// documented one was deprecated and its replacement is not offered to
// programs outside the system. What that means in practice is that this can
// stop working at an OS update, and the answer to that is to find out at
// startup and to refuse rather than to quietly stop confining anything.
func Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	info, err := os.Stat(applier())
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// Wrap turns a command into the same command, confined.
//
// The profile is written to a file rather than passed on the command line.
// An inline one is an argument, and a deployment with enough writable
// directories produces an argument longer than the kernel will accept — a
// failure that arrives as "argument list too long" from a program nobody
// typed.
//
// The caller removes the file when the command has finished; the returned
// function does that.
func Wrap(policy Policy, program string, args []string) (
	wrapped string, wrappedArgs []string, cleanup func(), err error,
) {
	if !Available() {
		return "", nil, nil, ErrUnavailable
	}

	profile, err := Profile(policy)
	if err != nil {
		return "", nil, nil, err
	}

	file, err := os.CreateTemp("", "jingclaw-sandbox-*.sb")
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandbox: write the profile: %w", err)
	}
	remove := func() { _ = os.Remove(file.Name()) }

	if _, err := file.WriteString(profile); err != nil {
		_ = file.Close()
		remove()
		return "", nil, nil, fmt.Errorf("sandbox: write the profile: %w", err)
	}
	if err := file.Close(); err != nil {
		remove()
		return "", nil, nil, fmt.Errorf("sandbox: write the profile: %w", err)
	}

	return applier(), append([]string{"-f", file.Name(), program}, args...), remove, nil
}

// LooksConfined reports whether a program refused because of the sandbox.
//
// sandbox-exec reports a policy refusal on stderr and exits non-zero, which
// on its own is indistinguishable from the program having failed. Said here
// so the observation the model gets can name the cause rather than leaving it
// to guess why "touch" did not work.
func LooksConfined(output string) bool {
	for _, said := range []string{
		"Operation not permitted",
		"sandbox-exec",
		"deny file-write",
		"deny network",
	} {
		if strings.Contains(output, said) {
			return true
		}
	}
	return false
}

// Profile renders the policy as a Seatbelt profile.
//
// Deliberately not deny-by-default. A profile that starts by forbidding
// everything and then permits what a compiler, a package manager, the
// keychain, the trust daemon and the Mach bootstrap server each need is not a
// small file, and every deployment that grows one grows it by finding out
// what broke. Starting from allow and subtracting the two things that matter
// — writing anywhere, and the network — is a policy somebody can read.
func Profile(policy Policy) (string, error) {
	var out strings.Builder

	out.WriteString("(version 1)\n\n")
	out.WriteString(";; Written by JingClaw. What it subtracts is what matters:\n")
	out.WriteString(";; nothing outside the workspace may be written, and\n")
	out.WriteString(";; nothing at all may be reached over the network.\n")
	out.WriteString("(allow default)\n\n")

	if !policy.Network {
		out.WriteString(";; No sockets of any kind.\n(deny network*)\n\n")
	}

	out.WriteString(";; The filesystem is readable and not writable.\n")
	out.WriteString("(deny file-write*)\n\n")

	// Re-opening the null device is common enough that forbidding it breaks
	// ordinary programs for no gain.
	out.WriteString("(allow file-write* (literal \"/dev/null\"))\n")

	for _, dir := range policy.Writable {
		resolved, err := resolve(dir)
		if err != nil {
			return "", err
		}
		out.WriteString("(allow file-write* (subpath " + quote(resolved) + "))\n")
	}

	if len(policy.Unreadable) > 0 {
		out.WriteString("\n;; Not shown to it at all.\n")
		for _, dir := range policy.Unreadable {
			resolved, err := resolve(dir)
			if err != nil {
				return "", err
			}
			out.WriteString("(deny file-read* (subpath " + quote(resolved) + "))\n")
		}
	}

	return out.String(), nil
}

// quote writes a path as a Seatbelt string literal.
//
// A path may contain a quote or a backslash — a directory called `it's "here"`
// is unusual and legal — and one written raw would end the string early and
// leave the rest of it as profile syntax.
func quote(path string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
}

// describeBackend is what Seatbelt offers, which is all of it.
func describeBackend() string { return "sandbox-exec (files and network)" }
