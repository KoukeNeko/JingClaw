package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A save that fails after its temporary file exists leaves neither the
// temporary file nor a changed destination: nothing is left behind to hold a
// secret.
func TestAFailedReplaceLeavesNoTemporaryFile(t *testing.T) {
	root := t.TempDir()
	dir, err := OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}

	// Moving a file over a directory is refused by every platform, and only
	// after the file has been written.
	destination := filepath.Join(dir.resolved, "gemini.key")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}

	err = dir.replace(destination, "canary-value")
	if err == nil {
		t.Fatal("a file was moved over a directory")
	}
	if strings.Contains(err.Error(), "canary-value") {
		t.Errorf("the failure repeats the value: %v", err)
	}

	entries, err := os.ReadDir(dir.resolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "gemini.key" || !entries[0].IsDir() {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the directory holds %v, want the directory that was there and nothing else", names)
	}
}
