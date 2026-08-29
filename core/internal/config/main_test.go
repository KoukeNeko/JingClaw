package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// isolated is the throwaway deployment these tests resolve to.
var isolated string

// TestMain keeps this package's tests away from any real deployment.
//
// These tests resolve the default configuration path and write to it, and the
// default is now always inside a deployment directory. Pointed at the real
// one, writing the fixture — which is what these tests are for — overwrites a
// running deployment's settings, and passes.
//
// Set here rather than in each test. Remembering to isolate is exactly what
// the test that once did that damage had not done, and a rule that has to be
// remembered per test is one the next test added will miss.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "jingclaw-config-test")
	if err != nil {
		panic(err)
	}
	isolated = filepath.Join(dir, home.DirName)

	if err := os.Setenv(home.EnvVar, isolated); err != nil {
		panic(err)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// The guard, asserted rather than assumed.
//
// The damage it prevents was done by a test that passed: writing its fixture
// to the default path was the point, and the path had quietly become a real
// deployment's. So this checks where the default actually leads, rather than
// checking that somebody set an environment variable.
func TestTheseTestsCannotReachARealDeployment(t *testing.T) {
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}

	if !strings.HasPrefix(path, isolated) {
		t.Fatalf("the default path is %s, outside the throwaway deployment at %s: "+
			"a test writing there could overwrite somebody's running one", path, isolated)
	}
}
