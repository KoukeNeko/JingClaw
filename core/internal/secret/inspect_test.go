package secret_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm/permtest"
	"github.com/KoukeNeko/JingClaw/core/internal/secret"
)

const canary = "canary-9f3a1c77-do-not-reveal-b2e0"

// Inspect is Load's own walk with the value taken out, so for every state a
// credential can be in, the two have to agree on whether it will be used and on
// whether it is refused. A status that says a key is fine when the daemon will
// not start with it is the failure this is here to rule out.
func TestInspectAgreesWithLoadInEveryState(t *testing.T) {
	const variable = "JINGCLAW_TEST_INSPECT_KEY"
	const other = "JINGCLAW_TEST_INSPECT_OTHER"
	t.Setenv(variable, "")
	t.Setenv(other, "")

	file := func(t *testing.T, content string, mutate func(path string)) string {
		t.Helper()
		path := writeKeyFile(t, content)
		if mutate != nil {
			mutate(path)
		}
		return path
	}

	cases := []struct {
		name   string
		env    map[string]string
		files  func(t *testing.T) []string
		source secret.Source
		state  secret.FileState
		named  string
		used   bool
		refuse bool
	}{
		{
			name: "nothing listed and nothing set", files: func(*testing.T) []string { return nil },
			source: secret.SourceNone, state: secret.FileNone,
		},
		{
			name: "a variable", env: map[string]string{variable: canary},
			files:  func(*testing.T) []string { return nil },
			source: secret.SourceEnvironment, state: secret.FileNone, named: variable, used: true,
		},
		{
			name: "the second variable when the first is blank", env: map[string]string{variable: "  ", other: canary},
			files:  func(*testing.T) []string { return nil },
			source: secret.SourceEnvironment, state: secret.FileNone, named: other, used: true,
		},
		{
			name: "a variable wins over a file that would be refused", env: map[string]string{variable: canary},
			files: func(t *testing.T) []string {
				return []string{file(t, canary, func(path string) { _ = permtest.Expose(path) })}
			},
			source: secret.SourceEnvironment, state: secret.FileNone, named: variable, used: true,
		},
		{
			name:   "a file",
			files:  func(t *testing.T) []string { return []string{file(t, canary+"\n", nil)} },
			source: secret.SourceFile, state: secret.FilePresent, used: true,
		},
		{
			name:   "a file with nothing in it",
			files:  func(t *testing.T) []string { return []string{file(t, " \n\t\n", nil)} },
			source: secret.SourceNone, state: secret.FileEmpty,
		},
		{
			name:   "no such file",
			files:  func(t *testing.T) []string { return []string{filepath.Join(t.TempDir(), "absent.key")} },
			source: secret.SourceNone, state: secret.FileAbsent,
		},
		{
			name: "a file that anybody could read",
			files: func(t *testing.T) []string {
				return []string{file(t, canary, func(path string) { _ = permtest.Expose(path) })}
			},
			source: secret.SourceBlocked, state: secret.FileWrongMode, refuse: true,
		},
		{
			name:   "a directory",
			files:  func(t *testing.T) []string { return []string{t.TempDir()} },
			source: secret.SourceBlocked, state: secret.FileNotRegular, refuse: true,
		},
		{
			// The size PutCredential and DeleteCredential stop at, so the three
			// agree on what is a credential file and a status cannot call present
			// what they will not touch.
			name: "a file bigger than any credential file",
			files: func(t *testing.T) []string {
				return []string{file(t, canary+strings.Repeat(" ", secret.MaxFileBytes), nil)}
			},
			source: secret.SourceBlocked, state: secret.FileUnreadable, refuse: true,
		},
		{
			name: "a name below something that is not a directory",
			files: func(t *testing.T) []string {
				return []string{filepath.Join(file(t, canary, nil), "below")}
			},
			source: secret.SourceBlocked, state: secret.FileUnreadable, refuse: true,
		},
		{
			name: "the second file when the first has nothing",
			files: func(t *testing.T) []string {
				return []string{file(t, "", nil), file(t, canary, nil)}
			},
			source: secret.SourceFile, state: secret.FilePresent, used: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			opts := secret.LoadOptions{EnvVars: []string{variable, other}, Files: tc.files(t)}

			verdict := secret.Inspect(opts, time.Now())
			if verdict.Source != tc.source || verdict.File != tc.state || verdict.EnvVar != tc.named {
				t.Errorf("verdict is %+v, want source %v, file %v, variable %q", verdict, tc.source, tc.state, tc.named)
			}

			loaded, err := secret.Load(opts)
			if (err != nil) != tc.refuse {
				t.Errorf("Load error is %v, want refusal %v", err, tc.refuse)
			}
			if loaded.IsSet() != tc.used {
				t.Errorf("Load set is %v, want %v", loaded.IsSet(), tc.used)
			}

			// And nothing it says carries the credential, whatever it is called.
			for _, rendered := range []string{
				fmt.Sprintf("%v", verdict), fmt.Sprintf("%+v", verdict), fmt.Sprintf("%#v", verdict),
				mustJSON(t, verdict),
			} {
				if strings.Contains(rendered, canary) {
					t.Errorf("the verdict repeats the credential: %s", rendered)
				}
			}
			if err != nil && strings.Contains(err.Error(), canary) {
				t.Errorf("the refusal repeats the credential: %v", err)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

func TestInspectSaysWhatTheModeIsAndNothingAboutTheFile(t *testing.T) {
	path := writeKeyFile(t, canary)
	if err := permtest.Expose(path); err != nil {
		t.Fatal(err)
	}

	verdict := secret.Inspect(secret.LoadOptions{Files: []string{path}}, time.Now())
	if verdict.File != secret.FileWrongMode || !strings.Contains(verdict.Detail, "mode") {
		t.Errorf("verdict is %+v, want the file's mode in the detail", verdict)
	}
	if strings.Contains(verdict.Detail, path) {
		t.Errorf("the detail names the path: %q", verdict.Detail)
	}
}

// The one thing taken from a file is when it was last modified, and whether a
// process that began before that holds what the file said then.
func TestInspectTakesTheModificationTimeAndNothingElse(t *testing.T) {
	path := writeKeyFile(t, canary)
	modified := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	opts := secret.LoadOptions{Files: []string{path}}

	before := secret.Inspect(opts, modified.Add(-time.Hour))
	if !before.ModifiedAt.Equal(modified) || !before.RestartNeeded {
		t.Errorf("a process that began before the file was written: %+v", before)
	}

	after := secret.Inspect(opts, modified.Add(time.Hour))
	if !after.ModifiedAt.Equal(modified) || after.RestartNeeded {
		t.Errorf("a process that began after the file was written: %+v", after)
	}

	// The variable is what is read, so the file is not what decides.
	t.Setenv("JINGCLAW_TEST_INSPECT_DECIDES", canary)
	overridden := secret.Inspect(secret.LoadOptions{
		EnvVars: []string{"JINGCLAW_TEST_INSPECT_DECIDES"}, Files: opts.Files,
	}, modified.Add(-time.Hour))
	if overridden.RestartNeeded {
		t.Errorf("a file the environment overrides is said to need a restart: %+v", overridden)
	}

	absent := secret.Inspect(secret.LoadOptions{Files: []string{path + ".absent"}}, modified)
	if !absent.ModifiedAt.IsZero() || absent.RestartNeeded {
		t.Errorf("a file that is not there has a time: %+v", absent)
	}
}
