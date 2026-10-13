package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func put(t *testing.T, dir, rel, content string) {
	t.Helper()

	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Digests already written into installed.json and staging records were made by
// one algorithm. The exported hasher is that algorithm, written out here by
// hand: a path, a NUL, a length, a NUL, the bytes, in sorted order.
func TestTheTreeDigestIsTheOneRecordsWereWrittenWith(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "b/second.txt", "two")
	put(t, dir, FileName, aSkill)
	put(t, dir, "a.txt", "hello")

	sum := sha256.New()
	var size int64
	for _, file := range []struct{ path, content string }{
		{"SKILL.md", aSkill}, {"a.txt", "hello"}, {"b/second.txt", "two"},
	} {
		fmt.Fprintf(sum, "%s\x00%d\x00%s", file.path, len(file.content), file.content)
		size += int64(len(file.content))
	}
	want := "sha256:" + hex.EncodeToString(sum.Sum(nil))

	got, gotSize, err := TreeDigest(dir)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got != want || gotSize != size {
		t.Errorf("digest %s (%d bytes), want %s (%d bytes)", got, gotSize, want, size)
	}
}

func TestFilesAreListedWithTheirHashes(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, FileName, aSkill)
	put(t, dir, "scripts/run.sh", "echo hi\n")
	put(t, dir, "logo.png", "\x89PNG\x00\x01binary")
	put(t, dir, "latin1.txt", "caf\xe9")

	files, err := Files(dir)
	if err != nil {
		t.Fatalf("files: %v", err)
	}

	hash := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}
	want := []File{
		{Path: "SKILL.md", Size: int64(len(aSkill)), SHA256: hash(aSkill), Text: true},
		{Path: "latin1.txt", Size: 4, SHA256: hash("caf\xe9"), Text: false},
		{Path: "logo.png", Size: 12, SHA256: hash("\x89PNG\x00\x01binary"), Text: false},
		{Path: "scripts/run.sh", Size: 8, SHA256: hash("echo hi\n"), Text: true},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files: %+v", len(files), files)
	}
	for index := range want {
		if files[index] != want[index] {
			t.Errorf("file %d is %+v, want %+v", index, files[index], want[index])
		}
	}
}

func TestATreeWithALinkHasNoDigestAndNoListing(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, FileName, aSkill)
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "host")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}

	if _, _, err := TreeDigest(dir); !errors.Is(err, ErrSource) {
		t.Errorf("a digest of a tree with a link: %v", err)
	}
	if _, err := Files(dir); !errors.Is(err, ErrSource) {
		t.Errorf("a listing of a tree with a link: %v", err)
	}

	// And a link standing in for the directory itself.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if _, _, err := TreeDigest(alias); err == nil {
		t.Error("a digest of a link to a directory")
	}
}
