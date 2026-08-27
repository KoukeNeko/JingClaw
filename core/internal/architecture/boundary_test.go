// Package architecture holds tests that assert structural invariants rather
// than behaviour. They fail when someone reaches across a boundary the design
// depends on, which is a class of mistake ordinary tests never catch.
package architecture_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The CLI is a control-plane client. If it can reach the runtime directly it
// will eventually be tempted to, and then there are two implementations of the
// agent loop that drift apart. The same rule protects the SwiftUI, WinUI and
// web clients, which cannot import Go at all — this test is the one place it
// can be mechanically enforced.
func TestCLIDoesNotDependOnRuntime(t *testing.T) {
	forbidden := []string{
		"github.com/KoukeNeko/JingClaw/core/internal/runtime",
		"github.com/KoukeNeko/JingClaw/core/internal/control",
		"github.com/KoukeNeko/JingClaw/core/internal/event",
		"github.com/KoukeNeko/JingClaw/core/internal/provider/fake",
	}

	deps := packageDeps(t, "github.com/KoukeNeko/JingClaw/core/cmd/agent")

	for _, pkg := range forbidden {
		if deps[pkg] {
			t.Errorf("cmd/agent must not depend on %s;\n"+
				"the CLI is a projection of the daemon, not a second runtime", pkg)
		}
	}
}

// The runtime must stay ignorant of the wire format, so that a second
// transport (or a replacement for Connect) is an additive change.
func TestRuntimeDoesNotDependOnGeneratedProtobuf(t *testing.T) {
	deps := packageDeps(t, "github.com/KoukeNeko/JingClaw/core/internal/runtime")

	for dep := range deps {
		if strings.HasPrefix(dep, "github.com/KoukeNeko/JingClaw/core/gen/") {
			t.Errorf("internal/runtime must not depend on generated code (%s);\n"+
				"translation belongs in internal/control", dep)
		}
	}
}

// Same rule one level deeper: domain types are the vocabulary every other
// package shares, so they must not be shaped by the protocol.
func TestDomainHasNoInternalDependencies(t *testing.T) {
	deps := packageDeps(t, "github.com/KoukeNeko/JingClaw/core/internal/domain")

	for dep := range deps {
		if strings.HasPrefix(dep, "github.com/KoukeNeko/JingClaw/core/") {
			t.Errorf("internal/domain must not depend on %s; it is the leaf package", dep)
		}
	}
}

// Provider adapters must depend on the contract, never on the runtime.
// Reversing that would let adding a vendor reach into run lifecycle, storage
// or permissions.
func TestProviderAdaptersDoNotDependOnRuntime(t *testing.T) {
	adapters := []string{
		"github.com/KoukeNeko/JingClaw/core/internal/provider",
		"github.com/KoukeNeko/JingClaw/core/internal/provider/fake",
		"github.com/KoukeNeko/JingClaw/core/internal/provider/gemini",
	}

	forbidden := []string{
		"github.com/KoukeNeko/JingClaw/core/internal/runtime",
		"github.com/KoukeNeko/JingClaw/core/internal/storage",
		"github.com/KoukeNeko/JingClaw/core/internal/control",
	}

	for _, adapter := range adapters {
		deps := packageDeps(t, adapter)
		for _, pkg := range forbidden {
			if deps[pkg] {
				t.Errorf("%s must not depend on %s", adapter, pkg)
			}
		}
	}
}

// Vendor SDK types must not escape their adapter. If they do, swapping a
// provider stops being a local change.
func TestVendorSDKStaysInsideItsAdapter(t *testing.T) {
	const vendorSDK = "google.golang.org/genai"

	for _, pkg := range []string{
		"github.com/KoukeNeko/JingClaw/core/internal/runtime",
		"github.com/KoukeNeko/JingClaw/core/internal/control",
		"github.com/KoukeNeko/JingClaw/core/internal/storage",
		"github.com/KoukeNeko/JingClaw/core/internal/provider",
		"github.com/KoukeNeko/JingClaw/core/cmd/agent",
	} {
		if packageDeps(t, pkg)[vendorSDK] {
			t.Errorf("%s depends on %s; the SDK belongs to internal/provider/gemini alone", pkg, vendorSDK)
		}
	}
}

func packageDeps(t *testing.T, pkg string) map[string]bool {
	t.Helper()

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}

	deps := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// go list -deps includes the package itself; only its dependencies
		// are of interest here.
		if line != "" && line != pkg {
			deps[line] = true
		}
	}
	return deps
}
