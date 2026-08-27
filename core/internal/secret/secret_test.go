package secret_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/secret"
)

const sentinel = "super-secret-api-key-value"

// A credential that reaches a log is a credential that has to be rotated, and
// the usual way it gets there is a struct printed with %v during debugging.
// These cases pin that shut.
func TestValueNeverPrintsItself(t *testing.T) {
	value := secret.New(sentinel)

	renderings := map[string]string{
		"%v":       fmt.Sprintf("%v", value),
		"%s":       fmt.Sprintf("%s", value),
		"%q":       fmt.Sprintf("%q", value),
		"%+v":      fmt.Sprintf("%+v", value),
		"String()": value.String(),
		"in struct": fmt.Sprintf("%v", struct {
			Key secret.Value
		}{Key: value}),
	}

	for verb, rendered := range renderings {
		if strings.Contains(rendered, sentinel) {
			t.Errorf("%s leaked the credential: %s", verb, rendered)
		}
	}
}

func TestValueIsRedactedInJSON(t *testing.T) {
	encoded, err := json.Marshal(struct {
		Key secret.Value `json:"key"`
	}{Key: secret.New(sentinel)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(encoded), sentinel) {
		t.Errorf("JSON leaked the credential: %s", encoded)
	}
}

func TestRevealReturnsTheValue(t *testing.T) {
	if got := secret.New(sentinel).Reveal(); got != sentinel {
		t.Errorf("Reveal returned %q", got)
	}
}

func TestLoadPrefersEnvironment(t *testing.T) {
	path := writeKeyFile(t, "from-file", 0o600)
	t.Setenv("JINGCLAW_TEST_KEY", "from-env")

	value, err := secret.Load(secret.LoadOptions{
		EnvVars: []string{"JINGCLAW_TEST_KEY"},
		Files:   []string{path},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value.Reveal() != "from-env" {
		t.Errorf("got %q, want the environment value", value.Reveal())
	}
}

func TestLoadFallsBackToFile(t *testing.T) {
	path := writeKeyFile(t, "  from-file\n", 0o600)

	value, err := secret.Load(secret.LoadOptions{
		EnvVars: []string{"JINGCLAW_TEST_UNSET_KEY"},
		Files:   []string{path},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Surrounding whitespace is a copy-paste artefact, not part of the key.
	if value.Reveal() != "from-file" {
		t.Errorf("got %q, want %q", value.Reveal(), "from-file")
	}
}

// A key other local accounts can read should be treated as compromised, so
// loading it is refused rather than silently accepted.
func TestLoadRefusesWorldReadableFile(t *testing.T) {
	path := writeKeyFile(t, "exposed", 0o644)

	_, err := secret.Load(secret.LoadOptions{Files: []string{path}})
	if err == nil {
		t.Fatal("loaded a world-readable credential without complaint")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
	if strings.Contains(err.Error(), "exposed") {
		t.Errorf("error leaked the credential: %v", err)
	}
}

// A missing credential is not an error here; the caller decides whether the
// feature it unlocks is required.
func TestLoadMissingFileIsNotAnError(t *testing.T) {
	value, err := secret.Load(secret.LoadOptions{
		Files: []string{filepath.Join(t.TempDir(), "absent.key")},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value.IsSet() {
		t.Error("reported a credential where there is none")
	}
}

func TestEmptyEnvVarFallsThrough(t *testing.T) {
	path := writeKeyFile(t, "from-file", 0o600)
	t.Setenv("JINGCLAW_TEST_KEY", "   ")

	value, err := secret.Load(secret.LoadOptions{
		EnvVars: []string{"JINGCLAW_TEST_KEY"},
		Files:   []string{path},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value.Reveal() != "from-file" {
		t.Errorf("a whitespace-only env var shadowed the file: %q", value.Reveal())
	}
}

func writeKeyFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.key")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	// WriteFile is subject to umask, so set the mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod key file: %v", err)
	}
	return path
}
