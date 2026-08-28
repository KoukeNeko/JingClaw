package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// fetchScript drives a browser and prints what one page says.
//
// Embedded rather than installed, so the daemon has one fewer file to lose and
// the script can never be a version behind the program that runs it.
//
//go:embed fetch.py
var fetchScript string

// BrowserFetcher fetches pages by driving a real browser.
//
// A plain HTTP client is the obvious implementation and, for a growing share of
// the web, the wrong one: a great many sites now answer anything that does not
// look like a browser with a challenge page, and the agent reads the challenge
// as the article. Driving a browser costs a process and some seconds per fetch
// and gets the page.
type BrowserFetcher struct {
	// Python is the interpreter to run. Empty means python3 from PATH.
	Python string

	Timeout  time.Duration
	MaxLinks int
}

const (
	defaultFetchTimeout = 45 * time.Second
	defaultMaxLinks     = 50

	// The child gets longer than the fetch itself: a browser that has been
	// told to stop still has to tear down a Chromium, and killing it in the
	// middle leaves the process behind.
	childGrace = 15 * time.Second
)

func (f *BrowserFetcher) Describe() string { return "browser" }

type fetchRequest struct {
	URL       string `json:"url"`
	TimeoutMS int64  `json:"timeout_ms"`
	MaxLinks  int    `json:"max_links"`
}

type fetchResponse struct {
	Status   int    `json:"status"`
	FinalURL string `json:"final_url"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	Links    []Link `json:"links"`
	Error    string `json:"error"`
}

func (f *BrowserFetcher) Fetch(ctx context.Context, url string) (Page, error) {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = defaultFetchTimeout
	}
	maxLinks := f.MaxLinks
	if maxLinks <= 0 {
		maxLinks = defaultMaxLinks
	}

	request, err := json.Marshal(fetchRequest{
		URL:       url,
		TimeoutMS: timeout.Milliseconds(),
		MaxLinks:  maxLinks,
	})
	if err != nil {
		return Page{}, err
	}

	response, err := f.run(ctx, request, timeout+childGrace)
	if err != nil {
		return Page{}, err
	}
	if response.Error != "" {
		return Page{}, fmt.Errorf("web: could not fetch %s: %s", url, response.Error)
	}

	return Page{
		RequestedURL: url,
		FinalURL:     response.FinalURL,
		Status:       response.Status,
		Title:        strings.TrimSpace(response.Title),
		Text:         CollapseWhitespace(response.Text),
		Links:        response.Links,
	}, nil
}

// run executes the script and decodes its one line of output.
func (f *BrowserFetcher) run(ctx context.Context, request []byte, deadline time.Duration) (fetchResponse, error) {
	python := f.Python
	if python == "" {
		python = "python3"
	}

	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// The script is passed on stdin to the interpreter's "-" rather than
	// written to a file. There is no temporary file to clean up, and nothing
	// on disk for another process to swap between writing and running it.
	command := exec.CommandContext(runCtx, python, "-c", fetchScript)
	command.Stdin = bytes.NewReader(request)
	command.Env = childEnvironment()

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.WaitDelay = 5 * time.Second

	runErr := command.Run()

	// stdout is checked before the exit status. The script reports its own
	// failures as JSON and exits non-zero for the same event, and the JSON
	// says what happened while the exit code says only that something did.
	var response fetchResponse
	if line := lastJSONLine(stdout.Bytes()); line != nil {
		if err := json.Unmarshal(line, &response); err == nil {
			return response, nil
		}
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return response, fmt.Errorf("web: the page did not load within %s", deadline)
	}

	var notFound *exec.Error
	if errors.As(runErr, &notFound) {
		return response, fmt.Errorf(
			"web: %s is not on this machine, so pages cannot be fetched; "+
				"install it, or set [web] backend = \"none\"", python)
	}
	if runErr != nil {
		return response, fmt.Errorf("web: the fetcher failed: %v: %s",
			runErr, firstLines(stderr.String(), 5))
	}

	return response, errors.New("web: the fetcher returned nothing")
}

// lastJSONLine finds the object the script printed.
//
// Python libraries print notices to stdout without asking, so the answer is
// not reliably the only thing there. It is reliably the last line that parses.
func lastJSONLine(output []byte) []byte {
	lines := bytes.Split(output, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		if json.Valid(line) {
			return line
		}
	}
	return nil
}

// childEnvironment is what the fetcher runs with.
//
// The daemon's own environment holds provider credentials, and this child is a
// browser that will load somebody else's JavaScript. It gets what it needs to
// find an interpreter and a home directory, and nothing else.
func childEnvironment() []string {
	keep := []string{
		"PATH", "HOME", "USERPROFILE", "TMPDIR", "TEMP", "TMP",
		"LANG", "LC_ALL", "SystemRoot", "ComSpec", "PATHEXT",
		// The browser is downloaded once and cached; without these the child
		// cannot find it and re-downloads Chromium on every fetch.
		"CLOAKBROWSER_DIR", "CLOAKGPT_DATA_DIR", "PLAYWRIGHT_BROWSERS_PATH",
	}

	env := make([]string, 0, len(keep)+2)
	for _, name := range keep {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, "PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1")
}

func firstLines(text string, limit int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > limit {
		lines = append(lines[:limit], "…")
	}
	return strings.Join(lines, "\n")
}

// PythonPath resolves the interpreter, so a misconfiguration is reported when
// the daemon starts rather than when the model first asks for a page.
func PythonPath(configured string) (string, error) {
	candidate := configured
	if candidate == "" {
		candidate = "python3"
	}

	if strings.ContainsRune(candidate, filepath.Separator) {
		info, err := os.Stat(candidate)
		if err != nil {
			return "", fmt.Errorf("web: %s: %w", candidate, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("web: %s is a directory", candidate)
		}
		return candidate, nil
	}

	found, err := exec.LookPath(candidate)
	if err != nil {
		return "", fmt.Errorf("web: %s is not on PATH: %w", candidate, err)
	}
	return found, nil
}
