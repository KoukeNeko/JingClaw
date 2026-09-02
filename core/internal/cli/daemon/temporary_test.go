package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The failure this warns about is silent and total: the database holds every
// conversation, approval and memory, and a directory the system clears takes
// all of it without an error anywhere.
func TestADatabaseInATemporaryPlaceIsNoticed(t *testing.T) {
	// What this system calls temporary, whatever it calls it. The Unix
	// literals below are checked as well where they mean something; on
	// Windows they are just an odd directory name on the system drive.
	temporary := []string{
		filepath.Join(os.TempDir(), "jingclaw", "data.db"),
	}
	if runtime.GOOS != "windows" {
		temporary = append(temporary,
			"/tmp/jingclaw/data.db",
			"/private/tmp/whatever/data.db",
			"/var/tmp/jingclaw.db",
		)
	}
	for _, path := range temporary {
		if _, ok := isTemporary(path); !ok {
			t.Errorf("%s was not recognised as temporary", path)
		}
	}

	// Somewhere that survives is left alone, because warning about every path
	// is the same as warning about none.
	permanent := []string{
		filepath.Join(os.Getenv("HOME"), "Library/Application Support/JingClaw/jingclaw.db"),
		"/var/lib/jingclaw/data.db",
		"/opt/jingclaw/data.db",
	}
	for _, path := range permanent {
		if where, ok := isTemporary(path); ok {
			t.Errorf("%s was called temporary because of %q", path, where)
		}
	}
}

// On macOS /tmp is a link to /private/tmp, so the same doomed directory has
// two spellings and both have to be caught.
func TestBothSpellingsOfTheSameDirectoryAreCaught(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("that link is a Unix arrangement; here they are two ordinary names")
	}

	for _, path := range []string{"/tmp/a/b.db", "/private/tmp/a/b.db"} {
		if _, ok := isTemporary(path); !ok {
			t.Errorf("%s was not recognised", path)
		}
	}
}

// A relative path is resolved before it is judged, or running from inside a
// temporary directory would go unnoticed.
func TestARelativePathIsResolvedFirst(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if _, ok := isTemporary("data.db"); !ok {
		t.Errorf("a relative path inside %s was not recognised", dir)
	}
}
