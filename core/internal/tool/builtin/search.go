package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// skippedDirs are never worth walking. They dominate the results of any search
// that includes them and almost never contain what was being looked for.
var skippedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
	".next":        true,
	"target":       true,
	".idea":        true,
	".vscode":      true,
	"DerivedData":  true,
}

// GlobFiles finds files by path pattern.
//
// It is implemented in pure Go rather than by shelling out to find or fd. A
// single static binary dropped onto a server has to work without asking what
// else is installed, and shelling out would also mean quoting model-supplied
// text into a command line.
type GlobFiles struct {
	Workspace *workspace.Workspace

	Limits Limits
}

func (t *GlobFiles) Spec() tool.Spec {
	return tool.Spec{
		Name: "glob_files",
		Description: "Find files in the workspace by glob pattern, e.g. **/*.go or internal/**/[a-z]*.md. " +
			"Returns paths relative to the workspace root.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Glob pattern. ** matches any number of directories."
    },
    "max_results": {
      "type": "integer",
      "minimum": 1,
      "maximum": 1000,
      "description": "Maximum paths to return. Defaults to 200."
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`),
		Level: tool.LevelWorkspaceRead,
		Capabilities: tool.Capabilities{
			Provenance:   domain.ProvenanceLocalUnknown,
			ReadFS:       true,
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	MaxResults int    `json:"max_results"`
}

func (t *GlobFiles) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args globArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	if !doublestar.ValidatePattern(args.Pattern) {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Use a pattern such as **/*.go.",
			"%q is not a valid glob pattern", args.Pattern)
	}

	limit := args.MaxResults
	if limit <= 0 {
		limit = t.Limits.withDefaults().GlobResults
	}

	matches, truncated, err := t.walk(ctx, args.Pattern, limit)
	if err != nil {
		return tool.Result{}, err
	}

	if len(matches) == 0 {
		return tool.Result{
			Content: fmt.Sprintf("No files match %q.", args.Pattern),
			Summary: fmt.Sprintf("glob %s: no matches", args.Pattern),
		}, nil
	}

	sort.Strings(matches)
	content := strings.Join(matches, "\n")
	if truncated {
		content += fmt.Sprintf("\n[stopped at %d matches; narrow the pattern for the rest]", limit)
	}

	return tool.Result{
		Content:   content,
		Summary:   fmt.Sprintf("glob %s: %d matches", args.Pattern, len(matches)),
		Truncated: truncated,
	}, nil
}

func (t *GlobFiles) walk(ctx context.Context, pattern string, limit int) ([]string, bool, error) {
	root := t.Workspace.Root()

	var (
		matches   []string
		truncated bool
	)

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is a fact about the filesystem, not a
			// reason to abandon the whole search.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if entry.IsDir() {
			if path != root && skippedDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)

		ok, matchErr := doublestar.Match(pattern, relative)
		if matchErr != nil || !ok {
			return nil
		}

		if len(matches) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		matches = append(matches, relative)
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return nil, false, ctx.Err()
	}

	return matches, truncated, nil
}

// Grep searches file contents.
type Grep struct {
	Workspace *workspace.Workspace

	Limits Limits
}

func (t *Grep) Spec() tool.Spec {
	return tool.Spec{
		Name: "grep",
		Description: "Search workspace file contents. Returns matching lines with their paths and line numbers, " +
			"so the result can be fed straight into read_file.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Text to find, or a regular expression when regex is true."
    },
    "regex": {
      "type": "boolean",
      "description": "Treat query as a Go regular expression. Defaults to false."
    },
    "case_sensitive": {
      "type": "boolean",
      "description": "Defaults to false."
    },
    "include": {
      "type": "string",
      "description": "Only search files matching this glob, e.g. **/*.go"
    },
    "max_results": {
      "type": "integer",
      "minimum": 1,
      "maximum": 500,
      "description": "Maximum matching lines to return. Defaults to 100."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`),
		Level: tool.LevelWorkspaceRead,
		Capabilities: tool.Capabilities{
			Provenance:   domain.ProvenanceLocalUnknown,
			ReadFS:       true,
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

type grepArgs struct {
	Query         string `json:"query"`
	Regex         bool   `json:"regex"`
	CaseSensitive bool   `json:"case_sensitive"`
	Include       string `json:"include"`
	MaxResults    int    `json:"max_results"`
}

func (t *Grep) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args grepArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}
	if args.Query == "" {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "query must not be empty")
	}

	matcher, err := compileMatcher(args)
	if err != nil {
		return tool.Result{}, err
	}

	if args.Include != "" && !doublestar.ValidatePattern(args.Include) {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Use a pattern such as **/*.go.",
			"%q is not a valid glob pattern", args.Include)
	}

	limit := args.MaxResults
	if limit <= 0 {
		limit = t.Limits.withDefaults().GrepResults
	}

	matches, filesSearched, truncated, searchErr := t.search(ctx, args, matcher, limit)
	if searchErr != nil {
		return tool.Result{}, searchErr
	}

	if len(matches) == 0 {
		return tool.Result{
			Content: fmt.Sprintf("No matches for %q in %d files.", args.Query, filesSearched),
			Summary: fmt.Sprintf("grep %s: no matches", args.Query),
		}, nil
	}

	content := strings.Join(matches, "\n")
	if truncated {
		content += fmt.Sprintf("\n[stopped at %d matches; narrow the query for the rest]", limit)
	}

	return tool.Result{
		Content:   content,
		Summary:   fmt.Sprintf("grep %s: %d matches", args.Query, len(matches)),
		Truncated: truncated,
	}, nil
}

func compileMatcher(args grepArgs) (func(string) bool, *tool.Error) {
	if !args.Regex {
		needle := args.Query
		if args.CaseSensitive {
			return func(line string) bool { return strings.Contains(line, needle) }, nil
		}
		lowered := strings.ToLower(needle)
		return func(line string) bool { return strings.Contains(strings.ToLower(line), lowered) }, nil
	}

	pattern := args.Query
	if !args.CaseSensitive {
		pattern = "(?i)" + pattern
	}

	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, tool.Errorf(tool.CodeInvalidArguments,
			"Fix the expression, or set regex to false to search for literal text.",
			"invalid regular expression: %v", err)
	}
	return compiled.MatchString, nil
}

func (t *Grep) search(
	ctx context.Context,
	args grepArgs,
	matches func(string) bool,
	limit int,
) (results []string, filesSearched int, truncated bool, err error) {
	root := t.Workspace.Root()

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if entry.IsDir() {
			if path != root && skippedDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		info, statErr := entry.Info()
		if statErr != nil || info.Size() > t.Limits.withDefaults().MaxSearchableFile {
			return nil
		}

		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)

		if args.Include != "" {
			ok, matchErr := doublestar.Match(args.Include, relative)
			if matchErr != nil || !ok {
				return nil
			}
		}

		filesSearched++

		fileMatches, stopped := grepFile(path, relative, matches, limit-len(results))
		results = append(results, fileMatches...)
		if stopped || len(results) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return nil, filesSearched, false, ctx.Err()
	}

	return results, filesSearched, truncated, nil
}

func grepFile(path, relative string, matches func(string) bool, remaining int) ([]string, bool) {
	if remaining <= 0 {
		return nil, true
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		found  []string
		lineNo int
	)

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		// A NUL byte means this is not text; abandon the file rather than
		// emit garbage into the model's context.
		if strings.ContainsRune(line, 0) {
			return nil, false
		}
		if !matches(line) {
			continue
		}

		// Long lines are usually minified output. Enough is kept to identify
		// the match without pasting a whole bundle.
		if len(line) > 300 {
			line = line[:300] + "…"
		}

		found = append(found, fmt.Sprintf("%s:%d: %s", relative, lineNo, strings.TrimSpace(line)))
		if len(found) >= remaining {
			return found, true
		}
	}

	return found, false
}
