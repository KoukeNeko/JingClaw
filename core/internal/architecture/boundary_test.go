// Package architecture holds tests that assert structural invariants rather
// than behaviour. They fail when someone reaches across a boundary the design
// depends on, which is a class of mistake ordinary tests never catch.
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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

// The settings and the two instruction documents are rows in the store, saved
// whole with a version history of their own. They are not events, and nothing
// that runs for the agent may be able to change the text that decides what it
// may do.
//
// Two places write them: a save through the control plane, which is the
// dashboard, and the daemon's start, which gives a document with no row its
// first text. go list says what a package imports and not what it calls, and
// the runtime imports the store as well, so this looks for the calls.
func TestOnlyTheControlPlaneAndStartupWriteTheSettings(t *testing.T) {
	allowed := map[string]bool{
		"internal/control":             true,
		"internal/cli/daemon":          true,
		"internal/storage/storagetest": true, // the contract both stores are held to
	}
	writes := map[string]bool{"PutDocument": true, "SeedDocuments": true}

	module, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, root := range []string{filepath.Join(module, "internal"), filepath.Join(module, "cmd")} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && writes[selector.Sel.Name] {
					dir, err := filepath.Rel(module, filepath.Dir(path))
					if err != nil {
						t.Fatal(err)
					}
					found[filepath.ToSlash(dir)] = true
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("read the source under %s: %v", root, err)
		}
	}

	for dir := range found {
		if !allowed[dir] {
			t.Errorf("%s writes a settings document;\n"+
				"only the control plane and the daemon's start do, so that a save is a person's decision "+
				"and not something a run, a tool or a client can do", dir)
		}
	}

	// A test that finds no writer asserts nothing, which is what it would see if
	// the methods were renamed.
	for _, dir := range []string{"internal/control", "internal/cli/daemon"} {
		if !found[dir] {
			t.Errorf("%s no longer calls PutDocument or SeedDocuments;\n"+
				"if the settings are written some other way now, this test is asserting nothing about it", dir)
		}
	}
}
