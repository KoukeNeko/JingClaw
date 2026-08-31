// Package architecture holds tests that assert structural invariants rather
// than behaviour. They fail when someone reaches across a boundary the design
// depends on, which is a class of mistake ordinary tests never catch.
package architecture_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The CLI is a control-plane client. If it can reach the runtime directly it
// will eventually be tempted to, and then there are two implementations of the
// agent loop that drift apart. The same rule protects any client that cannot
// import Go at all — this test is the one place it can be mechanically
// enforced.
//
// Asked of the package rather than of a binary. The daemon and the client are
// one program now, so the binary necessarily contains the runtime and the
// question "does the CLI reach it" can only be asked of internal/cli/client.
//
// This test spent several commits naming cmd/agent, which stopped existing
// when the programs were merged. It did not go red, because it shells out to
// go list and imports nothing: no edit anywhere invalidated its cached pass.
// A test that cannot notice its own subject is gone is worth less than no
// test, and that is why the package below is one this module builds.
func TestCLIDoesNotDependOnRuntime(t *testing.T) {
	forbidden := []string{
		"github.com/KoukeNeko/JingClaw/core/internal/runtime",
		"github.com/KoukeNeko/JingClaw/core/internal/control",
		"github.com/KoukeNeko/JingClaw/core/internal/event",
		"github.com/KoukeNeko/JingClaw/core/internal/provider/fake",
	}

	deps := packageDeps(t, clientPackage)

	for _, pkg := range forbidden {
		if deps[pkg] {
			t.Errorf("%s must not depend on %s;\n"+
				"the CLI is a projection of the daemon, not a second runtime",
				clientPackage, pkg)
		}
	}
}

// clientPackage is the CLI as a package, which is what the boundary is about.
const clientPackage = "github.com/KoukeNeko/JingClaw/core/internal/cli/client"

// The gateway talks to the runtime through a narrow interface it declares
// itself, so that what an untrusted channel can reach is a short list somebody
// can read rather than everything the agent loop exports.
//
// Importing the runtime for one error value is how that stops being true: the
// dependency arrives for something harmless and the next call is not.
func TestGatewayDoesNotDependOnRuntime(t *testing.T) {
	deps := packageDeps(t, "github.com/KoukeNeko/JingClaw/core/internal/gateway")

	if deps["github.com/KoukeNeko/JingClaw/core/internal/runtime"] {
		t.Error("internal/gateway must not depend on internal/runtime;\n" +
			"widen the DecidingRuntime interface, or put the shared vocabulary in internal/domain")
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
		clientPackage,
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
		// Named separately from any other failure. A package that has been
		// renamed or removed makes every assertion below vacuous, and the
		// useful thing to say is that the test no longer knows what it is
		// testing.
		t.Fatalf("go list -deps %s: %v\n"+
			"if that package was renamed or removed, this test has been asserting "+
			"nothing since it was; point it at what replaced it", pkg, err)
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

// TestTheSandboxCompilesEverywhereItClaimsTo is what keeps a platform from
// losing its answer silently.
//
// The package is split by build tag, and a split is a place where a function
// can exist on one platform and not another — which does not fail here, and
// does not fail in tests, and fails when somebody builds for the platform
// that lost it. Adding a backend means adding a file, and forgetting one
// means the whole program stops building for that platform.
//
// Only the sandbox, and deliberately: the rest of the program has a
// pre-existing gap on Windows that this is not the place to argue about.
func TestTheSandboxCompilesEverywhereItClaimsTo(t *testing.T) {
	// The caller, not only the package. Building the sandbox alone would
	// catch a file that does not compile and miss the thing that actually
	// happens: a platform where one of the three functions was never
	// written, which fails at whoever uses it.
	const caller = "github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"

	for _, target := range []struct{ os, arch string }{
		{"darwin", "arm64"},
		{"darwin", "amd64"},
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"windows", "amd64"},
	} {
		build := exec.Command("go", "build", "-buildvcs=false", "-o", os.DevNull, caller)
		build.Env = append(os.Environ(), "GOOS="+target.os, "GOARCH="+target.arch)

		if out, err := build.CombinedOutput(); err != nil {
			t.Errorf("what uses the sandbox does not build for %s/%s;\n"+
				"a backend is missing one of Available, Wrap or LooksConfined:\n%s",
				target.os, target.arch, out)
		}
	}
}
