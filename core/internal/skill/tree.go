package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

const (
	// MaxFileBytes, MaxTreeBytes and MaxFiles are the most a skill's directory may
	// be: one file in it, all of it, and how many files.
	//
	// A skill is instructions and the notes and scripts beside them, fetched from
	// anywhere, and everything that looks at one reads its files whole to hash
	// them. These are checked before anything is read, in the walk that every one
	// of them goes through, so that a repository cannot make a stage, a review or
	// a listing read more than this however it is arranged. A tree over them has
	// no digest, like one with a link in it; an installed skill that was staged
	// here is under them, and one that grew past them since is called edited.
	MaxFileBytes = 8 << 20
	MaxTreeBytes = 32 << 20
	MaxFiles     = 1000

	// MaxSkillMDBytes is the most SKILL.md may be. It is the instructions the
	// model is given and the text a person is shown to decide on, and it is
	// returned whole, so it has to fit in a reply.
	MaxSkillMDBytes = 1 << 20
)

// File is one file in a skill's directory, as it is on disk.
type File struct {
	// Path is relative to the skill's directory and uses slashes.
	Path string

	Size int64

	// SHA256 is the hash of the file's bytes, in hex.
	SHA256 string

	// Text is whether the file is UTF-8 with no NUL in it, which is what can
	// be shown to a person as text and what is worth reading before trusting
	// it.
	Text bool
}

// regularFiles lists every file under a directory in sorted order, and refuses
// the directory if anything in it is not a plain file.
//
// Looked at without following anything. A symbolic link in a fetched tree is
// the repository choosing what a later read of it lands on — a file anywhere
// on this machine — and the tree digest would hash the target, not the link,
// so nothing afterwards could tell the two apart. Refused here, in the one
// walk that both the digest and the file list go through, rather than in each.
//
// The same walk refuses a tree that is too big to be a skill, from the sizes
// the directory entries give and before a byte of any file is read, and a file
// whose name is not text: the listing is sent to a page as strings, and a skill
// whose files cannot be listed would otherwise still be there to be activated.
func regularFiles(dir string) ([]string, error) {
	// The directory itself is looked at the same way: a link standing in for it
	// is read as the directory it points at and moved as the link.
	var (
		files []string
		total int64
	)
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			what := "not a plain file"
			if entry.Type()&fs.ModeSymlink != 0 {
				what = "a symbolic link"
			}
			return because(ErrSource, fmt.Errorf(
				"skill: %s is %s; a skill is plain files and directories, because a link "+
					"would send whatever reads it to somewhere the skill's own bytes are not",
				filepath.ToSlash(rel), what))
		}
		if !utf8.ValidString(rel) {
			return because(ErrSource, fmt.Errorf(
				"skill: a file is called %q, which is not text; every file of a skill is listed by name, "+
					"and this one could not be", rel))
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		switch {
		case len(files) >= MaxFiles:
			return because(ErrSource, fmt.Errorf("skill: it has more than %d files", MaxFiles))
		case info.Size() > MaxFileBytes:
			return because(ErrSource, fmt.Errorf(
				"skill: %s is %d bytes; a file in a skill is at most %d", filepath.ToSlash(rel), info.Size(), MaxFileBytes))
		case total > MaxTreeBytes:
			return because(ErrSource, fmt.Errorf("skill: its files add up to more than %d bytes", MaxTreeBytes))
		}

		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(files)
	return files, nil
}

// TreeDigest hashes every file under a directory, so what is compared later is
// the whole skill and not one file in it.
//
// Deterministic: files are hashed in sorted path order, each preceded by its
// path and length, so a rename cannot pass for an edit and a file cannot be
// confused with the one after it.
//
// A tree with anything in it that is not a plain file has no digest.
func TreeDigest(dir string) (digest string, size int64, err error) {
	files, err := regularFiles(dir)
	if err != nil {
		return "", 0, err
	}

	sum := sha256.New()
	for _, rel := range files {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", 0, fmt.Errorf("skill: hashing %s: %w", dir, err)
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", rel, len(content))
		sum.Write(content)
		size += int64(len(content))
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil)), size, nil
}

// Files lists what a skill's directory holds, with each file's hash, in the
// order the tree digest takes them.
func Files(dir string) ([]File, error) {
	paths, err := regularFiles(dir)
	if err != nil {
		return nil, err
	}

	files := make([]File, 0, len(paths))
	for _, rel := range paths {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("skill: reading %s: %w", dir, err)
		}
		sum := sha256.Sum256(content)
		files = append(files, File{
			Path:   rel,
			Size:   int64(len(content)),
			SHA256: hex.EncodeToString(sum[:]),
			Text:   utf8.Valid(content) && !bytes.ContainsRune(content, 0),
		})
	}
	return files, nil
}
