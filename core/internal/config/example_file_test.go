package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// exampleFile is the copy checked into the repository, where somebody browsing
// it can read every setting without running anything.
//
// Under docs/ rather than beside the source. A "template" sitting where a
// config file could be read is one somebody will eventually edit and wonder
// why nothing changed — which is the whole failure the deployment directory
// was rearranged to prevent.
const exampleFile = "docs/config.example.toml"

// The checked-in copy is a derived artifact, the same as the generated
// protobuf code: one source of truth, and a test that catches it drifting.
//
// Without this, adding a setting updates the const, passes every other test,
// and leaves the file people actually open describing an older program.
func TestCheckedInExampleMatchesTheSource(t *testing.T) {
	path := filepath.Join("..", "..", "..", exampleFile)

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", exampleFile, err)
	}

	if string(onDisk) != config.Example {
		t.Errorf("%s no longer matches config.Example; regenerate it with:\n"+
			"\tgo run ./cmd/jingclaw daemon --print-config > ../%s", exampleFile, exampleFile)
	}
}
