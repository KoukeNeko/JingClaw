package secret_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm/permtest"
	"github.com/KoukeNeko/JingClaw/core/internal/secret"
)

// deployment is a directory of its own with a Directory opened on it, and a
// neighbour outside it that nothing here may touch.
type deployment struct {
	root    string
	outside string
	dir     secret.Directory
}

func newDeployment(t *testing.T) deployment {
	t.Helper()

	base := t.TempDir()
	root, outside := filepath.Join(base, "deployment"), filepath.Join(base, "outside")
	for _, path := range []string{root, outside} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := secret.OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	return deployment{root: root, outside: outside, dir: dir}
}

// listing is every name in a directory, so that a test can say "and nothing
// else" about what a call left behind.
func listing(t *testing.T, dir string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return strings.Join(names, ",")
}

func read(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func ownerOnly(t *testing.T, path string) bool {
	t.Helper()

	ok, _, err := fsperm.EnsureOwnerOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestWriteMakesTheFileOwnerOnlyWithExactlyTheValueAndLeavesNothingBeside(t *testing.T) {
	d := newDeployment(t)

	if err := d.dir.Write("gemini.key", "  "+canary+"\n", time.Time{}); err != nil {
		t.Fatalf("write: %v", err)
	}

	path := filepath.Join(d.root, "gemini.key")
	if got := read(t, path); got != canary {
		t.Errorf("the file holds %q, want the value with the whitespace around it taken off", got)
	}
	if !ownerOnly(t, path) {
		t.Error("the file can be read by others")
	}
	if got := listing(t, d.root); got != "gemini.key" {
		t.Errorf("the directory holds %q, want only the file: no backup, no temporary", got)
	}
	if got := listing(t, d.outside); got != "" {
		t.Errorf("something was written outside: %q", got)
	}
}

// A file saved with the wrong mode is the commonest reason there is no agent,
// and saving the key again is the repair. Refusing it on its mode would send a
// person to a shell to do what this exists for.
func TestWriteReplacesAFileAnybodyCouldRead(t *testing.T) {
	d := newDeployment(t)
	path := filepath.Join(d.root, "gemini.key")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := permtest.Expose(path); err != nil {
		t.Fatal(err)
	}

	if err := d.dir.Write("gemini.key", "new", time.Time{}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := read(t, path); got != "new" || !ownerOnly(t, path) {
		t.Errorf("the file holds %q, owner-only %v", got, ownerOnly(t, path))
	}
	if got := listing(t, d.root); got != "gemini.key" {
		t.Errorf("the directory holds %q, want only the file", got)
	}
}

func TestWriteRefusesAValueThatCannotBeACredentialWithoutRepeatingIt(t *testing.T) {
	d := newDeployment(t)
	path := filepath.Join(d.root, "gemini.key")
	if err := os.WriteFile(path, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string]string{
		"empty":               "",
		"nothing but spaces":  " \t\r\n ",
		"a newline inside":    canary + "\n" + canary,
		"a carriage return":   canary + "\r" + canary,
		"a NUL":               canary + "\x00" + canary,
		"not UTF-8":           canary + "\xff\xfe" + canary,
		"too long":            canary + strings.Repeat("x", secret.MaxValueBytes),
		"longer by one":       strings.Repeat("y", secret.MaxValueBytes+1),
		"a line break inside": "a\u0085b\nc",
	} {
		err := d.dir.Write("gemini.key", value, time.Time{})
		if !errors.Is(err, secret.ErrInvalidValue) {
			t.Errorf("%s: got %v, want a refusal of the value", name, err)
			continue
		}
		if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "yyyy") {
			t.Errorf("%s: the refusal repeats the value: %v", name, err)
		}
	}

	if got := read(t, path); got != "kept" {
		t.Errorf("a refused save changed the file to %q", got)
	}

	// The most it takes, to the byte, is accepted.
	if err := d.dir.Write("gemini.key", strings.Repeat("z", secret.MaxValueBytes), time.Time{}); err != nil {
		t.Errorf("a value of exactly the limit is refused: %v", err)
	}
	if got := listing(t, d.root); got != "gemini.key" {
		t.Errorf("the directory holds %q", got)
	}
}

func TestWriteRefusesAValueWithoutTouchingTheFile(t *testing.T) {
	d := newDeployment(t)
	path := filepath.Join(d.root, "gemini.key")
	if err := os.WriteFile(path, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := d.dir.Write("gemini.key", "a\nb", time.Time{}); err == nil {
		t.Fatal("a value with a line break was written")
	}
	if got := read(t, path); got != "kept" {
		t.Errorf("a refused save changed the file to %q", got)
	}
	if got := listing(t, d.root); got != "gemini.key" {
		t.Errorf("a refused save left %q", got)
	}
}

// A link at the file's name is somebody's arrangement, not a credential file:
// writing through it would change what it points at, and replacing it would
// undo their arrangement. Either way the call is refused.
func TestACredentialCallRefusesALinkAtTheFileAndLeavesBothEndsAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege most Windows accounts do not have")
	}

	d := newDeployment(t)
	target := filepath.Join(d.outside, "elsewhere.key")
	if err := os.WriteFile(target, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d.root, "gemini.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() error{
		"write":    func() error { return d.dir.Write("gemini.key", "new", time.Time{}) },
		"remove":   func() error { _, err := d.dir.Remove("gemini.key"); return err },
		"restrict": func() error { _, err := d.dir.Restrict("gemini.key"); return err },
	} {
		if err := call(); !errors.Is(err, secret.ErrNotCredentialFile) {
			t.Errorf("%s: got %v, want the link refused", name, err)
		}
	}

	if got := read(t, target); got != "not yours" {
		t.Errorf("the target now holds %q", got)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("the target's mode changed: %v %v", info, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link is gone: %v %v", info, err)
	}
	if got := listing(t, d.root); got != "gemini.key" {
		t.Errorf("the directory holds %q", got)
	}
}

func TestACredentialCallRefusesWhatIsNotShapedLikeOne(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"a directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a file too large": func(t *testing.T, path string) {
			writeFile(t, path, strings.Repeat("a", secret.MaxFileBytes+1))
		},
		"a file of several lines": func(t *testing.T, path string) {
			writeFile(t, path, "[provider]\nbackend = \"fake\"\n")
		},
		"a file with a NUL": func(t *testing.T, path string) { writeFile(t, path, "a\x00b") },
		"a file that is not UTF-8": func(t *testing.T, path string) {
			writeFile(t, path, "\xff\xfe\xfd")
		},
	}

	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDeployment(t)
			path := filepath.Join(d.root, "gemini.key")
			arrange(t, path)

			before, _ := os.Lstat(path)
			for call, run := range map[string]func() error{
				"write":    func() error { return d.dir.Write("gemini.key", "new", time.Time{}) },
				"remove":   func() error { _, err := d.dir.Remove("gemini.key"); return err },
				"restrict": func() error { _, err := d.dir.Restrict("gemini.key"); return err },
			} {
				if err := run(); !errors.Is(err, secret.ErrNotCredentialFile) {
					t.Errorf("%s: got %v, want it refused", call, err)
				}
			}

			after, err := os.Lstat(path)
			if err != nil || after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Errorf("the file was changed: %v then %v (%v)", before, after, err)
			}
			if got := listing(t, d.root); got != "gemini.key" {
				t.Errorf("the directory holds %q", got)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A line ending an editor added is not a second line.
func TestAFileWithATrailingLineEndingIsStillACredentialFile(t *testing.T) {
	for _, content := range []string{"key\n", "key\r\n", "key", "", "  key  \n"} {
		d := newDeployment(t)
		path := filepath.Join(d.root, "gemini.key")
		writeFile(t, path, content)

		if err := d.dir.Write("gemini.key", "new", time.Time{}); err != nil {
			t.Errorf("%q: %v", content, err)
		}
	}
}

// What a name may be is the rule's business and is checked before this is
// reached. This is the second line behind it, and holds whatever the rule lets
// through: a write never lands anywhere but directly inside the directory.
func TestAWriteNeverLandsOutsideTheDirectoryWhateverTheName(t *testing.T) {
	d := newDeployment(t)

	for _, name := range []string{
		"../outside/escaped.key", "..", ".", "", "sub/escaped.key", "/abs.key", "escaped.key/", "./escaped.key",
		"../deployment/escaped.key",
	} {
		if err := d.dir.Write(name, canary, time.Time{}); err == nil {
			t.Errorf("%q was written", name)
		}
		if _, err := d.dir.Remove(name); err == nil {
			t.Errorf("%q was removed", name)
		}
		if _, err := d.dir.Restrict(name); err == nil {
			t.Errorf("%q was restricted", name)
		}
	}

	for _, dir := range []string{d.root, d.outside, filepath.Dir(d.root)} {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "escaped") || strings.HasPrefix(entry.Name(), ".credential") {
				t.Errorf("%s holds %s", dir, entry.Name())
			}
		}
	}
}

// The directory is resolved once, links followed, so what is checked is where
// the write goes.
func TestADirectoryReachedThroughALinkIsWrittenAtItsRealPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege most Windows accounts do not have")
	}

	d := newDeployment(t)
	through := filepath.Join(filepath.Dir(d.root), "through")
	if err := os.Symlink(d.root, through); err != nil {
		t.Fatal(err)
	}

	dir, err := secret.OpenDirectory(through)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Write("gemini.key", canary, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(d.root, "gemini.key")); got != canary {
		t.Errorf("the real directory holds %q", got)
	}
}

func TestOpenDirectoryRefusesWhatIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, file, "x")

	if _, err := secret.OpenDirectory(file); err == nil {
		t.Error("a file was opened as the deployment directory")
	}
	if _, err := secret.OpenDirectory(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory that is not there: %v", err)
	}
}

// The compare-and-set is on the modification time and nothing from the content:
// there is nothing here that could hash a file.
func TestWriteExpectsTheModificationTimeItWasToldAndNothingElse(t *testing.T) {
	d := newDeployment(t)
	path := filepath.Join(d.root, "gemini.key")
	writeFile(t, path, "first")
	// Whole seconds, which every file system keeps.
	modified := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}

	if err := d.dir.Write("gemini.key", "stale", modified.Add(time.Second)); !errors.Is(err, secret.ErrChanged) {
		t.Errorf("a time the file does not have: %v", err)
	}
	if got := read(t, path); got != "first" {
		t.Errorf("a refused save changed the file to %q", got)
	}

	if err := d.dir.Write("gemini.key", "fresh", modified); err != nil {
		t.Errorf("the time the file has: %v", err)
	}
	if got := read(t, path); got != "fresh" {
		t.Errorf("the file holds %q", got)
	}

	if err := d.dir.Write("absent.key", "x", modified); !errors.Is(err, secret.ErrChanged) {
		t.Errorf("a file that is gone, expected as it was: %v", err)
	}
	if got := listing(t, d.root); got != "gemini.key" {
		t.Errorf("the directory holds %q", got)
	}
}

// A person who opened the box when there was no file is not shown one that
// has appeared by the time they save, and must not overwrite it unseen.
func TestWriteNewLeavesAFileThatAppearedAlone(t *testing.T) {
	d := newDeployment(t)
	path := filepath.Join(d.root, "gemini.key")

	if err := d.dir.WriteNew("gemini.key", "first"); err != nil {
		t.Fatalf("no file yet: %v", err)
	}
	if got := read(t, path); got != "first" {
		t.Errorf("the file holds %q", got)
	}

	if err := d.dir.WriteNew("gemini.key", "second"); !errors.Is(err, secret.ErrChanged) {
		t.Errorf("a file that is there: %v", err)
	}
	if got := read(t, path); got != "first" {
		t.Errorf("a refused save changed the file to %q", got)
	}
}

func TestRemoveDeletesACredentialFileAndIsContentWithNone(t *testing.T) {
	d := newDeployment(t)
	writeFile(t, filepath.Join(d.root, "gemini.key"), canary)

	removed, err := d.dir.Remove("gemini.key")
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	removed, err = d.dir.Remove("gemini.key")
	if err != nil || removed {
		t.Errorf("removing what is not there: %v %v", removed, err)
	}
	if got := listing(t, d.root); got != "" {
		t.Errorf("the directory holds %q", got)
	}
}

func TestRestrictFixesAFileAnybodyCouldReadWithoutTheValue(t *testing.T) {
	d := newDeployment(t)
	path := filepath.Join(d.root, "gemini.key")
	writeFile(t, path, canary)
	if err := permtest.Expose(path); err != nil {
		t.Fatal(err)
	}

	changed, err := d.dir.Restrict("gemini.key")
	if err != nil || !changed || !ownerOnly(t, path) {
		t.Fatalf("restrict: %v %v owner-only %v", changed, err, ownerOnly(t, path))
	}
	if got := read(t, path); got != canary {
		t.Errorf("the content changed to %q", got)
	}

	changed, err = d.dir.Restrict("gemini.key")
	if err != nil || changed {
		t.Errorf("restricting a file that already is: %v %v", changed, err)
	}
	if _, err := d.dir.Restrict("absent.key"); !errors.Is(err, secret.ErrNoFile) {
		t.Errorf("restricting a file that is not there: %v", err)
	}
}
