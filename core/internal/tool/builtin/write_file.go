package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// Observer records what the agent has actually seen on disk.
//
// It exists so a write can refuse to clobber a file the model never read, and
// to notice a file that changed after it was read. The model is not asked to
// supply a hash: it would forget, and a rule the model has to remember is not
// a rule.
type Observer struct {
	mu   sync.RWMutex
	seen map[string]string // absolute path -> content hash when last read
}

func NewObserver() *Observer {
	return &Observer{seen: make(map[string]string)}
}

func (o *Observer) Observe(path, hash string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen[path] = hash
}

func (o *Observer) Seen(path string) (string, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	hash, ok := o.seen[path]
	return hash, ok
}

// Forget drops an observation, used after a write so the recorded hash cannot
// go stale against the file the agent itself just changed.
func (o *Observer) Forget(path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.seen, path)
}

func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// WriteFile creates a file or replaces one in full.
//
// Its scope is deliberately narrow: new files, and small existing ones. A
// targeted edit is a different operation with different failure modes, and
// conflating the two is how an agent ends up regenerating two thousand lines
// to change one of them.
type WriteFile struct {
	Workspace *workspace.Workspace
	Observer  *Observer

	// Locks is shared with EditFile so the two cannot interleave on one file.
	Locks *keyedMutex

	Limits Limits
}

func NewWriteFile(ws *workspace.Workspace, observer *Observer, locks *keyedMutex) *WriteFile {
	return &WriteFile{Workspace: ws, Observer: observer, Locks: locks}
}

func (t *WriteFile) Spec() tool.Spec {
	return tool.Spec{
		Name: "write_file",
		Description: "Create a new file, or replace a small existing file in full. " +
			"An existing file must have been read first. For anything large or for a targeted change, read the " +
			"relevant range instead of rewriting the whole file.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path relative to the workspace root."
    },
    "content": {
      "type": "string",
      "description": "The complete new contents of the file."
    }
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`),
		Level: tool.LevelWorkspaceWrite,
		Capabilities: tool.Capabilities{
			ReadFS:  true,
			WriteFS: true,
			// Replacing a file's contents cannot be undone by calling this
			// again with different arguments.
			Destructive: true,
			Idempotent:  true,
		},
	}
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFile) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args writeFileArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	absolute, err := t.Workspace.Resolve(args.Path)
	if err != nil {
		return tool.Result{}, pathError(args.Path, err)
	}

	release := t.Locks.Lock(absolute)
	defer release()

	if !utf8.ValidString(args.Content) {
		return tool.Result{}, tool.Errorf(tool.CodeUnsupported,
			"Write valid UTF-8 text.",
			"the content for %s is not valid UTF-8", args.Path)
	}

	existing, existed, err := t.inspectExisting(args.Path, absolute)
	if err != nil {
		return tool.Result{}, err
	}

	content := args.Content
	if existed {
		// Preserve how the file is written down. Silently dropping a BOM or
		// rewriting every line ending turns a one-line change into a
		// whole-file diff that nobody asked for.
		if shape, ok := parseTextFile(existing); ok {
			content = shape.Render(strings.ReplaceAll(content, "\r\n", "\n"))
		}
	}

	if err := atomicWrite(absolute, []byte(content)); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	// The file on disk is now what this call wrote, so the recorded
	// observation is updated rather than dropped: a follow-up edit should not
	// have to read the file again to change what the agent itself just made.
	t.Observer.Observe(absolute, hashBytes([]byte(content)))

	verb := "created"
	if existed {
		verb = "replaced"
	}
	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++
	}

	return tool.Result{
		Content: fmt.Sprintf("%s %s (%d lines, %d bytes)", verb, args.Path, lines, len(content)),
		Summary: fmt.Sprintf("%s %s", verb, args.Path),
	}, nil
}

// inspectExisting enforces the rules that protect an existing file.
func (t *WriteFile) inspectExisting(relative, absolute string) ([]byte, bool, error) {
	info, err := os.Stat(absolute)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}
	if info.IsDir() {
		return nil, false, tool.Errorf(tool.CodeInvalidArguments,
			"Choose a file path, not a directory.",
			"%s is a directory", relative)
	}
	if info.Size() > t.Limits.withDefaults().MaxOverwriteBytes {
		return nil, false, tool.Errorf(tool.CodeTooLarge,
			"Read the region you need to change and make a targeted edit instead.",
			"%s is %d bytes, too large to replace in full", relative, info.Size())
	}

	existing, err := os.ReadFile(absolute)
	if err != nil {
		return nil, false, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	observed, seen := t.Observer.Seen(absolute)
	if !seen {
		// Overwriting a file the agent has never looked at destroys content it
		// cannot describe, which is the single most damaging thing a coding
		// agent does.
		return nil, false, tool.Errorf(tool.CodePermissionDenied,
			"Read the file first, then write it.",
			"%s already exists and has not been read in this session", relative)
	}

	if current := hashBytes(existing); current != observed {
		// Something changed the file after it was read: a human, an editor,
		// another agent. Writing now would silently discard that change.
		return nil, false, tool.Errorf(tool.CodeInvalidArguments,
			"Read the file again to see the current contents, then rewrite it.",
			"%s changed on disk since it was read", relative)
	}

	return existing, true, nil
}

// atomicWrite replaces a file's contents without ever leaving it half-written.
//
// The temporary file is created in the same directory so the rename stays
// within one filesystem; a rename across devices is not atomic and would
// degrade to a copy.
func atomicWrite(path string, content []byte) error {
	dir := filepath.Dir(path)

	temp, err := os.CreateTemp(dir, ".jingclaw-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempName := temp.Name()

	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}

	if _, err := temp.Write(content); err != nil {
		cleanup()
		return fmt.Errorf("write temporary file: %w", err)
	}

	// Flush before the rename: on a crash, a renamed but unflushed file can
	// appear as a correctly named empty one.
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("flush temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("close temporary file: %w", err)
	}

	// Keep the original's permissions; a new file gets the usual default
	// rather than the restrictive mode CreateTemp uses.
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tempName, mode); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("set permissions: %w", err)
	}

	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("replace file: %w", err)
	}

	return nil
}

// Preview is what would be written.
//
// A whole file rather than a diff, because this tool replaces rather than
// edits and the thing to review is what will be there afterwards. Nothing is
// read from disk: this runs before a decision, and a preview with a side
// effect is the write happening without approval — which also means it cannot
// say whether the file already exists.
func (t *WriteFile) Preview(arguments json.RawMessage) string {
	var args writeFileArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		return ""
	}
	if args.Content == "" {
		return ""
	}

	var out strings.Builder
	fmt.Fprintf(&out, "+++ %s\n", args.Path)
	writePrefixed(&out, "+", boundPreview(args.Content))
	return out.String()
}

// maxPreviewBytes is where a preview stops being something a person reads
// before deciding and starts being a file they scroll past. Somebody who wants
// the whole of it can read the file after approving.
const maxPreviewBytes = 8 << 10

func boundPreview(text string) string {
	if len(text) <= maxPreviewBytes {
		return text
	}

	runes := []rune(text)
	if len(runes) > maxPreviewBytes {
		runes = runes[:maxPreviewBytes]
	}
	return string(runes) + "\n… (the rest is not shown)"
}
