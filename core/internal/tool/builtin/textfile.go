package builtin

import (
	"strings"
	"sync"
	"unicode/utf8"
)

// utf8BOM is the byte order mark. Some Windows tooling writes it, and dropping
// it on a write is a whole-file diff nobody asked for.
const utf8BOM = "\uFEFF"

// textFile is a file's contents decomposed into the parts an edit must
// preserve and the part it may change.
//
// Line endings and a BOM are not content: they are how the file is written
// down. A model editing one function has no way to see them and no business
// changing them, so they are stripped before matching and restored on write.
type textFile struct {
	// Body is the content with LF endings and no BOM, which is what edits are
	// matched against.
	Body string

	hadBOM bool
	crlf   bool
}

func parseTextFile(raw []byte) (textFile, bool) {
	if !utf8.Valid(raw) {
		return textFile{}, false
	}

	content := string(raw)

	file := textFile{}
	if strings.HasPrefix(content, utf8BOM) {
		file.hadBOM = true
		content = strings.TrimPrefix(content, utf8BOM)
	}

	// A file counts as CRLF when it uses those endings at all. Mixed endings
	// exist, and normalising the whole file to match the majority would be the
	// gratuitous rewrite this is meant to avoid; the dominant style is only
	// used for lines the edit introduces.
	if strings.Contains(content, "\r\n") {
		file.crlf = true
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}

	file.Body = content
	return file, true
}

// Render writes the body back out in the file's original shape.
func (f textFile) Render(body string) string {
	if f.crlf {
		body = strings.ReplaceAll(body, "\n", "\r\n")
	}
	if f.hadBOM {
		body = utf8BOM + body
	}
	return body
}

// keyedMutex serialises work per key.
//
// Read-verify-write on a file is only safe when nothing else touches that file
// in between. A single global lock would serialise unrelated files; one lock
// per path keeps concurrent edits to different files independent.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu sync.Mutex

	// waiters counts holders and hopefuls, so the entry can be dropped once
	// nobody wants it and the map does not grow for the life of the process.
	waiters int
}

// NewFileLocks creates the lock set shared by the writing tools. They must
// share one: separate sets would let a write and an edit to the same file run
// at once, each verifying against contents the other is replacing.
func NewFileLocks() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*keyedLock)}
}

// Lock acquires the lock for a key and returns the release function.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	entry, ok := k.locks[key]
	if !ok {
		entry = &keyedLock{}
		k.locks[key] = entry
	}
	entry.waiters++
	k.mu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()

		k.mu.Lock()
		entry.waiters--
		if entry.waiters == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// countOccurrences returns how many times needle appears in haystack, stopping
// once the count passes limit so a common substring does not cost a full scan.
func countOccurrences(haystack, needle string, limit int) int {
	if needle == "" {
		return 0
	}

	count := 0
	for offset := 0; ; {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			return count
		}
		count++
		if count > limit {
			return count
		}
		offset += index + len(needle)
	}
}

// lineOf returns the 1-based line number at a byte offset.
func lineOf(content string, offset int) int {
	if offset > len(content) {
		offset = len(content)
	}
	return strings.Count(content[:offset], "\n") + 1
}
