package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The name comes from the media type, not from anything the agent chose.
//
// What an extension does is pick the program that opens the file. Taking one
// from a tool's own output would let a run decide which program runs on the
// operator's machine, which is a larger thing than showing them a log.
func TestTheNameComesFromTheMediaTypeAndNotFromTheAgent(t *testing.T) {
	extension, openable := ExtensionFor("text/plain")
	if !openable || extension != ".txt" {
		t.Errorf("text/plain is named %q, openable=%v", extension, openable)
	}

	// Parameters are not part of the type. A log refused for saying its
	// encoding is a log nobody can read.
	if extension, openable := ExtensionFor("text/plain; charset=utf-8"); !openable ||
		extension != ".txt" {
		t.Errorf("a type with parameters is named %q, openable=%v", extension, openable)
	}
}

// A type nobody should be handed to a default program is refused.
//
// The refusal is the point rather than an inconvenience: an artifact is
// whatever a tool produced, which includes whatever a page the run read
// suggested it produce. Handing that to the machine's default program for it
// is running somebody else's file.
func TestATypeThatShouldNotBeOpenedIsRefused(t *testing.T) {
	for _, refused := range []string{
		"application/x-mach-binary",
		"application/x-sh",
		"application/octet-stream",
		"",
	} {
		if _, openable := ExtensionFor(refused); openable {
			t.Errorf("%q would be handed to the machine", refused)
		}
	}
}

// What is written is never executable.
//
// A file mode is not a judgement about the contents, and this is the
// difference between opening a document and running one.
func TestWhatIsWrittenIsNeverExecutable(t *testing.T) {
	into := t.TempDir()

	path, err := WriteForOpening(into, "sha256-abc", ".txt", []byte("the build log"))
	if err != nil {
		t.Fatalf("writing it out: %v", err)
	}
	if written, err := os.ReadFile(path); err != nil || string(written) != "the build log" {
		t.Fatalf("what was written is %q (%v)", written, err)
	}

	about, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if about.Mode().Perm()&0o111 != 0 {
		t.Errorf("the file handed to the machine is executable: %v", about.Mode())
	}
}

// And writing over one that is already there still leaves it unrunnable.
//
// The mode passed to a write applies when the file is created and not when it
// is replaced. These names are a digest and an extension, and the directory
// they go in outlives a run, so the second time an artifact is opened is a
// write over a file that is already there.
func TestWritingOverAnExistingFileStillLeavesItUnrunnable(t *testing.T) {
	into := t.TempDir()

	path := filepath.Join(into, "sha256-abc.txt")
	if err := os.WriteFile(path, []byte("stale"), 0o755); err != nil {
		t.Fatalf("staging: %v", err)
	}

	written, err := WriteForOpening(into, "sha256-abc", ".txt", []byte("the build log"))
	if err != nil {
		t.Fatalf("writing over it: %v", err)
	}

	about, err := os.Stat(written)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if about.Mode().Perm()&0o111 != 0 {
		t.Errorf("the file written over an executable one is still executable: %v",
			about.Mode())
	}
}

// An id cannot become a path.
//
// An artifact id is a digest and looks nothing like a path, which is exactly
// why this is checked: the day one does not, a name with a slash in it writes
// wherever the slash points, and the console is the thing holding the pen.
func TestAnIdCannotBecomeAPath(t *testing.T) {
	for _, id := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"sha256-abc/../../..",
		"..",
	} {
		named := safeName(id)
		if strings.ContainsAny(named, `/\`) || strings.Contains(named, "..") {
			t.Errorf("%q became %q, which still points somewhere", id, named)
		}
	}

	if named := safeName("sha256-abc123"); named != "sha256-abc123" {
		t.Errorf("an ordinary id was rewritten to %q", named)
	}
	if named := safeName("!!!"); named == "" {
		t.Error("an id of nothing but punctuation left no name at all")
	}
}
