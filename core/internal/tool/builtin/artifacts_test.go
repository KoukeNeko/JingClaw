package builtin_test

import (
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// newArchivingFixture is a workspace whose exec_command keeps what it cannot
// show, and a read_artifact to get it back.
func newArchivingFixture(t *testing.T, maxOutput int) *tool.Registry {
	t.Helper()

	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	store, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}

	limits := builtin.Limits{MaxCommandOutput: maxOutput, ReadLimit: 4096}

	registry := tool.NewRegistry()
	registry.MustRegister(
		&builtin.ExecCommand{Workspace: ws, Limits: limits, Artifacts: store},
		&builtin.ReadArtifact{Artifacts: store, Limits: limits},
	)

	return registry
}

// countingCommand prints numbered lines, so a test can tell whether the part
// it is looking at is the part it asked for.
func countingCommand(lines int) (string, []string) {
	script := "i=0; while [ $i -lt " + itoa(lines) + " ]; do echo \"line-$i-padding-padding-padding\"; i=$((i+1)); done"
	if runtime.GOOS == "windows" {
		// for /L counts inclusively, so [0, lines-1] matches the shell loop's
		// half-open [0, lines): the same line-0 through line-(lines-1).
		return "cmd.exe", []string{"/d", "/s", "/c",
			"for /L %i in (0,1," + itoa(lines-1) + ") do @echo line-%i-padding-padding-padding"}
	}
	return "/bin/sh", []string{"-c", script}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

var artifactID = regexp.MustCompile(`sha256-[0-9a-f]{64}`)

// Truncation without an artifact is destruction: the model is told there was
// more and given no way to reach it.
func TestTruncatedOutputIsKeptAndTheModelIsToldWhere(t *testing.T) {
	registry := newArchivingFixture(t, 500)

	program, args := countingCommand(200)
	result := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args,
	})

	if !result.Truncated {
		t.Fatalf("200 lines through a 500-byte bound was not truncated:\n%s", result.Content)
	}
	if result.Artifact == nil {
		t.Fatal("the whole output was not kept")
	}
	if result.Artifact.Size <= 500 {
		t.Errorf("the artifact is %d bytes, no larger than what was shown", result.Artifact.Size)
	}

	// The model has to be able to find the id in what it was shown, or the
	// artifact might as well not exist.
	found := artifactID.FindString(result.Content)
	if found == "" {
		t.Fatalf("the content does not name an artifact:\n%s", result.Content)
	}
	if found != result.Artifact.ID {
		t.Errorf("the content names %s but the result carries %s", found, result.Artifact.ID)
	}
	if !strings.Contains(result.Content, "read_artifact") {
		t.Error("the model is not told how to read the rest")
	}
}

// The point of keeping it is getting it back, including the middle that the
// excerpt dropped.
func TestTheMissingMiddleCanBeReadBack(t *testing.T) {
	registry := newArchivingFixture(t, 500)

	program, args := countingCommand(200)
	produced := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args,
	})
	if produced.Artifact == nil {
		t.Fatal("nothing was kept")
	}

	// A line from the middle, which by construction is what the excerpt cut.
	const middle = "line-100-padding"
	if strings.Contains(produced.Content, middle) {
		t.Skip("the excerpt happened to include the middle; nothing to prove here")
	}

	whole := readWholeArtifact(t, registry, produced.Artifact.ID, produced.Artifact.Size)
	if !strings.Contains(whole, middle) {
		t.Errorf("the part the excerpt dropped is not in the artifact")
	}
	for _, want := range []string{"line-0-", "line-199-"} {
		if !strings.Contains(whole, want) {
			t.Errorf("the artifact is missing %q", want)
		}
	}
}

// readWholeArtifact pages through, which is also a test that paging terminates.
func readWholeArtifact(t *testing.T, registry *tool.Registry, id string, size int64) string {
	t.Helper()

	var (
		whole  strings.Builder
		offset int64
	)

	for range 100 {
		result := call(t, registry, "read_artifact", map[string]any{
			"artifact_id": id,
			"offset":      offset,
			"limit":       512,
		})
		if result.IsError {
			t.Fatalf("read_artifact failed: %s", result.Content)
		}

		body, next, done := parseWindow(t, result.Content)
		whole.WriteString(body)
		if done {
			break
		}
		if next <= offset {
			t.Fatalf("paging did not advance past offset %d", offset)
		}
		offset = next
	}

	if int64(whole.Len()) != size {
		t.Errorf("paging produced %d bytes, want %d", whole.Len(), size)
	}
	return whole.String()
}

var (
	windowHeader = regexp.MustCompile(`^bytes (\d+)-(\d+) of (\d+)\n`)
	windowFooter = regexp.MustCompile(`\n\[\d+ bytes remain; read again from offset \d+\]$`)
)

// parseWindow pulls apart what read_artifact returns, which is also a check
// that a caller can: a window nobody can locate is a window nobody can page.
func parseWindow(t *testing.T, content string) (body string, next int64, done bool) {
	t.Helper()

	header := windowHeader.FindStringSubmatch(content)
	if header == nil {
		t.Fatalf("read_artifact did not say which bytes these are:\n%s", content)
	}

	body = windowFooter.ReplaceAllString(strings.TrimPrefix(content, header[0]), "")

	end := atoi(t, header[2])
	total := atoi(t, header[3])

	return body, end, end >= total
}

func atoi(t *testing.T, text string) int64 {
	t.Helper()

	var n int64
	for _, r := range text {
		if r < '0' || r > '9' {
			t.Fatalf("%q is not a number", text)
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// An id the model invented, or one from a store that no longer has it, are
// different problems with different next steps.
func TestReadingSomethingThatIsNotThereSaysWhy(t *testing.T) {
	registry := newArchivingFixture(t, 500)

	invented := call(t, registry, "read_artifact", map[string]any{
		"artifact_id": "not-an-id-at-all",
	})
	if !invented.IsError {
		t.Error("an invented id was accepted")
	}
	if !strings.Contains(invented.Content, string(tool.CodeInvalidArguments)) {
		t.Errorf("the model is not told the id was malformed: %s", invented.Content)
	}

	absent := call(t, registry, "read_artifact", map[string]any{
		"artifact_id": "sha256-" + strings.Repeat("0", 64),
	})
	if !absent.IsError {
		t.Error("a well-formed id for nothing was accepted")
	}
	if !strings.Contains(absent.Content, string(tool.CodeNotFound)) {
		t.Errorf("the model is not told it is missing rather than malformed: %s", absent.Content)
	}
}

// Output that fits is not stored. An artifact per command would fill a disk
// with things nobody will read.
func TestOutputThatFitsIsNotStored(t *testing.T) {
	registry := newArchivingFixture(t, 64*1024)

	program, args := echoCommand("short")
	result := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args,
	})

	if result.Artifact != nil {
		t.Errorf("output that fitted was stored anyway as %s", result.Artifact.ID)
	}
}

// Without a store the tool still works; it just says plainly that the rest is
// gone rather than implying it is a call away.
func TestWithoutAStoreTheModelIsToldTheRestIsGone(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	registry := tool.NewRegistry()
	registry.MustRegister(&builtin.ExecCommand{
		Workspace: ws,
		Limits:    builtin.Limits{MaxCommandOutput: 500},
	})

	program, args := countingCommand(200)
	result := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args,
	})

	if result.Artifact != nil {
		t.Fatal("something was stored without a store")
	}
	if !strings.Contains(result.Content, "not recoverable") {
		t.Errorf("the model is left to assume the rest can be fetched:\n%s", result.Content)
	}
}
