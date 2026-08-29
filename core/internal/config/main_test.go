package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// TestMain keeps this package's tests away from any real deployment.
//
// These tests resolve the default configuration path and write to it. Once a
// .JingClaw directory decides that path, "the default location" became
// whatever exists above the checkout — and one of these tests overwrote a
// running deployment's configuration with its own fixture, and passed, because
// writing the fixture is what it was for.
//
// Set here rather than in each test. Remembering to isolate is exactly what
// the test that did the damage had not done, and a rule that has to be
// remembered per test is one that will be missed by the next one added.
func TestMain(m *testing.M) {
	if err := os.Setenv(home.EnvVar, "none"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// The guard, asserted rather than assumed.
//
// The damage it prevents was done by a test that passed: writing its fixture
// to the default path was the point, and the path had quietly become a real
// deployment's. So this checks that the default path cannot be one, which is
// the property that was violated, rather than checking that somebody set an
// environment variable.
func TestTheseTestsCannotReachARealDeployment(t *testing.T) {
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}

	if strings.Contains(path, home.DirName) {
		t.Fatalf("the default path is %s, inside a %s directory: a test writing "+
			"there would overwrite somebody's running deployment", path, home.DirName)
	}
}
