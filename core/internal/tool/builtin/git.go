package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// Reading a repository is its own pair of tools rather than exec_command,
// because it is asked for constantly and risks nothing: "what have I changed"
// is the question before and after every edit, and going through the shell
// means an approval every time for something that only reads.
//
// Only reading. Committing, pushing and rebasing stay with exec_command,
// where they are approved: their consequences leave the machine, and the
// approval semantics are a different problem from this one.
//
// The arguments are fixed here rather than passed through. A tool that ran
// `git <whatever the model said>` at read level would be a way to run any git
// subcommand without approval, which is most of a shell.

const (
	// gitTimeout bounds a read of the repository. A status that has not
	// answered in this long is a repository something else is holding a lock
	// on, and waiting longer does not help.
	gitTimeout = 30 * time.Second

	// maxGitOutput is what goes in front of the model. A diff longer than
	// this is kept whole as an artifact and named, the same as a build log.
	maxGitOutput = 24 << 10
)

// gitSafety are the settings that stop reading a repository from running
// programs the repository chose.
//
// A diff can invoke an external diff driver or a textconv filter, both
// configured inside the repository being read. That turns "show me what
// changed" into "run whatever this repository says", which is exactly what a
// tool that needs no approval must not do.
var gitSafety = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "diff.external=",
	"--no-optional-locks",
}

// GitStatus reports what has changed in the workspace's repository.
type GitStatus struct {
	Workspace *workspace.Workspace
}

func (t *GitStatus) Spec() tool.Spec {
	return tool.Spec{
		Name: "git_status",
		Description: "Report the workspace's git branch and which files are staged, " +
			"changed or untracked. Reads only; use exec_command for anything that changes " +
			"the repository.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`),
		Level:        tool.LevelWorkspaceRead,
		Capabilities: tool.Capabilities{ReadFS: true, Idempotent: true},
	}
}

func (t *GitStatus) Execute(ctx context.Context, _ tool.Call) (tool.Result, error) {
	// Porcelain v2 is the documented machine-readable form, and the one that
	// does not change between git versions. Parsing the human output is how a
	// tool breaks on somebody else's machine.
	out, err := runGit(ctx, t.Workspace.Root(), "status", "--porcelain=v2", "--branch")
	if err != nil {
		return tool.Result{}, err
	}

	status := parseStatus(out)
	return tool.Result{
		Content: renderStatus(status),
		Summary: summariseStatus(status),
	}, nil
}

// GitDiff shows what changed, as a diff.
type GitDiff struct {
	Workspace *workspace.Workspace

	// Artifacts is where a diff too large to show is kept. Left nil, a large
	// diff is simply cut.
	Artifacts *artifact.Store
}

func (t *GitDiff) Spec() tool.Spec {
	return tool.Spec{
		Name: "git_diff",
		Description: "Show what has changed in the workspace's repository, as a diff. " +
			"By default the changes that are not staged; set staged to see what is. " +
			"Reads only.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "staged": {
      "type": "boolean",
      "description": "Show what is staged for commit instead of what is not."
    },
    "path": {
      "type": "string",
      "description": "Limit to one file or directory, relative to the workspace root."
    },
    "context_lines": {
      "type": "integer",
      "minimum": 0,
      "maximum": 20,
      "description": "Lines of context around each change. Defaults to 3."
    }
  },
  "additionalProperties": false
}`),
		Level:        tool.LevelWorkspaceRead,
		Capabilities: tool.Capabilities{ReadFS: true, Idempotent: true},
	}
}

type gitDiffArgs struct {
	Staged       bool   `json:"staged"`
	Path         string `json:"path"`
	ContextLines *int   `json:"context_lines"`
}

func (t *GitDiff) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args gitDiffArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	context_ := 3
	if args.ContextLines != nil {
		context_ = *args.ContextLines
	}

	// Every flag is chosen here. --no-ext-diff and --no-textconv are the two
	// that matter: both would otherwise run a program the repository named.
	argv := []string{
		"diff", "--no-color", "--no-ext-diff", "--no-textconv",
		fmt.Sprintf("--unified=%d", context_),
	}
	if args.Staged {
		argv = append(argv, "--staged")
	}

	if args.Path != "" {
		// Resolved through the workspace so a path cannot reach outside it,
		// and put after -- so it cannot be read as a flag however it is
		// spelled.
		if _, err := t.Workspace.Resolve(args.Path); err != nil {
			return tool.Result{}, pathError(args.Path, err)
		}
		argv = append(argv, "--", args.Path)
	}

	out, err := runGit(ctx, t.Workspace.Root(), argv...)
	if err != nil {
		return tool.Result{}, err
	}

	if strings.TrimSpace(out) == "" {
		where := "not staged"
		if args.Staged {
			where = "staged"
		}
		return tool.Result{
			Content: fmt.Sprintf("Nothing %s has changed.", where),
			Summary: "no changes",
		}, nil
	}

	shown, truncated := boundOutput([]byte(out), maxGitOutput)

	// A diff that did not fit is the one somebody most wants whole: it is
	// large because a lot changed.
	var stored *tool.Artifact
	if truncated {
		ref, storeErr := archive(ctx, t.Artifacts, []byte(out), "text/x-diff")
		stored = ref
		shown += noteArtifact(ref, storeErr)
	}

	return tool.Result{
		Content:   shown,
		Summary:   summariseDiff(out),
		Truncated: truncated,
		Artifact:  stored,
	}, nil
}

// runGit runs one fixed command in the workspace.
func runGit(ctx context.Context, root string, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	argv := append(append([]string{}, gitSafety...), args...)
	command := exec.CommandContext(runCtx, "git", argv...)
	command.Dir = root
	command.Env = minimalEnvironment(false)
	command.Stdin = nil

	var out, problems bytes.Buffer
	command.Stdout = &out
	command.Stderr = &problems

	err := command.Run()
	if err == nil {
		return out.String(), nil
	}

	var missing *exec.Error
	if errors.As(err, &missing) {
		return "", tool.Errorf(tool.CodeUnsupported,
			"Use exec_command if git is installed somewhere unusual.",
			"git is not installed on this machine")
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return "", tool.Errorf(tool.CodeTimeout,
			"Something may be holding the repository's lock.",
			"git did not answer within %s", gitTimeout)
	}

	said := strings.TrimSpace(problems.String())
	if strings.Contains(said, "not a git repository") {
		return "", tool.Errorf(tool.CodeNotFound,
			"", "the workspace is not a git repository")
	}
	return "", tool.Errorf(tool.CodeInternal, "", "git failed: %s", firstLine(said))
}

func firstLine(text string) string {
	if at := strings.IndexByte(text, '\n'); at >= 0 {
		return text[:at]
	}
	if text == "" {
		return "no output"
	}
	return text
}

// repositoryStatus is what git said, in a shape worth rendering.
type repositoryStatus struct {
	Branch   string
	Upstream string
	Ahead    int
	Behind   int

	Staged    []string
	Changed   []string
	Untracked []string
	Conflicts []string
}

// parseStatus reads porcelain v2.
//
// The format is documented and stable: header lines start with "# ", a
// changed entry with "1" or "2", a conflict with "u", and an untracked file
// with "?". The two-character XY field says whether the change is staged, not
// staged, or both.
func parseStatus(out string) repositoryStatus {
	var status repositoryStatus

	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "# branch.head "):
			status.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.upstream "):
			status.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			fmt.Sscanf(strings.TrimPrefix(line, "# branch.ab "), "+%d -%d",
				&status.Ahead, &status.Behind)

		case strings.HasPrefix(line, "? "):
			status.Untracked = append(status.Untracked, strings.TrimPrefix(line, "? "))

		case strings.HasPrefix(line, "u "):
			if path := lastField(line); path != "" {
				status.Conflicts = append(status.Conflicts, path)
			}

		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			path := lastField(line)
			if path == "" {
				continue
			}
			// XY: the first character is the staged change, the second is
			// the one that is not. A file can be in both lists, and saying so
			// is the point — it is how a partly-staged file looks.
			state := fields[1]
			if len(state) == 2 {
				if state[0] != '.' {
					status.Staged = append(status.Staged, path)
				}
				if state[1] != '.' {
					status.Changed = append(status.Changed, path)
				}
			}
		}
	}

	return status
}

// lastField is the path at the end of a porcelain entry.
//
// A rename entry carries two paths separated by a tab, and the one that
// matters is where the file is now.
func lastField(line string) string {
	if at := strings.IndexByte(line, '\t'); at >= 0 {
		line = line[:at]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func renderStatus(status repositoryStatus) string {
	var out strings.Builder

	branch := status.Branch
	if branch == "" || branch == "(detached)" {
		branch = "no branch (detached head)"
	}
	fmt.Fprintf(&out, "on %s", branch)
	if status.Upstream != "" {
		fmt.Fprintf(&out, ", tracking %s", status.Upstream)
		if status.Ahead > 0 || status.Behind > 0 {
			fmt.Fprintf(&out, " (%d ahead, %d behind)", status.Ahead, status.Behind)
		}
	}
	out.WriteString("\n")

	// Conflicts first: nothing else in the list matters while there are any.
	writeFileList(&out, "unmerged", status.Conflicts)
	writeFileList(&out, "staged", status.Staged)
	writeFileList(&out, "changed, not staged", status.Changed)
	writeFileList(&out, "untracked", status.Untracked)

	if len(status.Conflicts)+len(status.Staged)+len(status.Changed)+len(status.Untracked) == 0 {
		out.WriteString("nothing has changed")
	}

	return out.String()
}

// maxListedFiles is where a list stops being readable. A status naming four
// hundred files is one nobody reads, and the count is the useful part.
const maxListedFiles = 40

func writeFileList(out *strings.Builder, label string, files []string) {
	if len(files) == 0 {
		return
	}

	fmt.Fprintf(out, "\n%s (%d):\n", label, len(files))
	shown := files
	if len(shown) > maxListedFiles {
		shown = shown[:maxListedFiles]
	}
	for _, file := range shown {
		fmt.Fprintf(out, "  %s\n", file)
	}
	if len(files) > len(shown) {
		fmt.Fprintf(out, "  … and %d more\n", len(files)-len(shown))
	}
}

func summariseStatus(status repositoryStatus) string {
	parts := make([]string, 0, 4)
	if len(status.Conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("%d unmerged", len(status.Conflicts)))
	}
	if len(status.Staged) > 0 {
		parts = append(parts, fmt.Sprintf("%d staged", len(status.Staged)))
	}
	if len(status.Changed) > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", len(status.Changed)))
	}
	if len(status.Untracked) > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", len(status.Untracked)))
	}

	branch := status.Branch
	if branch == "" {
		branch = "detached"
	}
	if len(parts) == 0 {
		return branch + ": clean"
	}
	return branch + ": " + strings.Join(parts, ", ")
}

// summariseDiff counts what a diff touches, for a timeline that shows one
// line per tool call.
func summariseDiff(diff string) string {
	var files, added, removed int
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files++
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			// Headers, not content.
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return fmt.Sprintf("%d file(s), +%d -%d", files, added, removed)
}
