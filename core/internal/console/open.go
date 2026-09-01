package console

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Opener hands a file to whatever the machine opens that kind with.
//
// An interface so the checks can watch what would be opened without anything
// actually being launched, and because "the machine" is a different program
// on every platform.
//
// Here rather than drawn in the log, because a terminal is a poor image
// viewer and a worse PDF reader. What somebody wants when a build fails is
// the log in the thing they read logs in.
type Opener interface {
	Open(ctx context.Context, path string) error
}

// openableTypes is what may be handed to a default program.
//
// An allowlist, and short on purpose. An artifact is whatever a tool
// produced, which includes whatever a page the run read suggested it produce;
// handing that to the machine's default program for it is running somebody
// else's file. What is here is what somebody plausibly needs to look at
// while deciding: a log, a picture, a document.
var openableTypes = map[string]string{
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"application/json": ".json",
	"application/pdf":  ".pdf",
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/gif":        ".gif",
	"image/webp":       ".webp",
}

// ExtensionFor says how a stored output may be named, and whether it may be
// opened at all.
//
// The extension comes from the media type and never from anything the agent
// chose. What an extension does is pick the program that runs, so taking one
// from a tool's own output would let a run decide which program starts on the
// operator's machine — a larger thing than showing them a log.
func ExtensionFor(mediaType string) (string, bool) {
	// Parameters dropped: "text/plain; charset=utf-8" is text/plain, and a
	// lookup that missed it would refuse a log for saying its encoding.
	base := strings.TrimSpace(strings.ToLower(mediaType))
	if semicolon := strings.IndexByte(base, ';'); semicolon >= 0 {
		base = strings.TrimSpace(base[:semicolon])
	}

	extension, allowed := openableTypes[base]
	return extension, allowed
}

// TheMachine is the platform's own "open this".
type TheMachine struct{}

// Open hands the path over, without waiting for whatever opens it.
//
// Not waited on because the program that opens a PDF stays open, and a panel
// blocked until somebody closes their reader would look hung.
func (TheMachine) Open(ctx context.Context, path string) error {
	opener, args := openerFor(path)
	if opener == "" {
		return fmt.Errorf("console: no way to open a file on %s", runtime.GOOS)
	}

	command := exec.CommandContext(ctx, opener, args...)
	if err := command.Start(); err != nil {
		return fmt.Errorf("console: opening %s: %w", filepath.Base(path), err)
	}

	// Released rather than waited on. Waiting would hold the panel until the
	// reader is closed; not releasing would leave a zombie for each one.
	go func() { _ = command.Wait() }()
	return nil
}

// openerFor is the platform's own "open this".
func openerFor(path string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{path}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", path}
	default:
		return "xdg-open", []string{path}
	}
}

// WriteForOpening puts the bytes where something else can read them.
//
// Never executable, whatever it is. A file mode is not a judgement about the
// contents, and 0o600 on a file about to be handed to a default program is
// the difference between opening a document and running one.
func WriteForOpening(dir, id, extension string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("console: making somewhere to put it: %w", err)
	}

	path := filepath.Join(dir, safeName(id)+extension)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("console: writing it out: %w", err)
	}

	// Said again, because the mode above applies only when the file is
	// created. These names are a digest and an extension and this directory
	// outlives a run, so writing over one that is already there keeps
	// whatever mode it already had — and the promise this function makes is
	// about the file that is handed over, not about the first one.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("console: setting how it may be used: %w", err)
	}
	return path, nil
}

// safeName keeps an id from becoming a path.
//
// An artifact id is a digest and looks nothing like a path, which is exactly
// why this is here: the day one does not, a name with a slash in it writes
// wherever the slash points.
func safeName(id string) string {
	kept := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, id)

	if kept == "" {
		return "output"
	}
	return kept
}
