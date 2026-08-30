// Package sandbox confines what an approved command can reach.
//
// A human approval and a sandbox answer different questions:
//
//	approval  authorises intent   — did somebody mean to run npm install?
//	sandbox   authorises effects  — and what will its 1,200 dependencies do?
//
// The first is answerable by a person. The second is not: approving a build
// is approving the execution of a great deal of code nobody has read, and no
// amount of care at the approval makes that untrue.
//
// So the gain is uneven and worth stating plainly. For "git status" this is
// defence in depth and little more. For "make", "./configure", "go generate"
// or "npm install" it is most of the protection there is.
//
// What it does not do: a command allowed the network and able to read a
// secret can still send it; a workspace is writable and so can be spoiled;
// and anything written now can be run outside this later.
package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Policy is what one execution is allowed to reach.
type Policy struct {
	// Writable are the directories the command may change. Everything else
	// on the machine is readable and not writable.
	Writable []string

	// Unreadable are directories to hide, whatever else is allowed.
	//
	// Confinement of writes says nothing about reads: a sandboxed command can
	// still open ~/.ssh, and "it has no network" only means it cannot send
	// what it found today. So the places worth not showing it are named.
	Unreadable []string

	// Network allows outbound connections. Off is the ordinary case: most of
	// what an agent runs is a build or a test, and neither needs one.
	Network bool
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

// resolve is the path the kernel will see.
//
// Symlinks followed, because the sandbox matches on what a path resolves to
// and not on what was written. On macOS this is not an edge case: /tmp and
// /var are both links into /private, so a workspace under either is a
// workspace the profile permits and the kernel does not recognise — and what
// that looks like is a command that cannot write to the directory it was
// given.
//
// A path that does not exist yet is kept as it was written. There is nothing
// to resolve, and refusing would mean a directory could not be permitted
// before it was made.
func resolve(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve %s: %w", dir, err)
	}

	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return absolute, nil
	}
	return real, nil
}

// quote writes a path as a Seatbelt string literal.
//
// A path may contain a quote or a backslash — a directory called `it's "here"`
// is unusual and legal — and one written raw would end the string early and
// leave the rest of it as profile syntax.
func quote(path string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
}
