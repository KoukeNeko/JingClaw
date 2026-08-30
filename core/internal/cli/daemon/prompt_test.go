package daemon

import (
	"reflect"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
)

// TestTheRuntimesToolsCanBeRegisteredBeforeItExists is what keeps the prompt
// naming every tool.
//
// The tools whose collaborator is the runtime used to be registered after it
// was built, which is after the prompt was assembled from the registry — so
// four of them were never named to the model, including the one for
// delegating. They are now registered against a handle instead, and this is
// the check that the handle still satisfies what each of them asks for.
//
// A compile-time assertion rather than a behavioural one: what went wrong was
// an ordering nobody could see, and what stops it is the tools not being able
// to be registered late in the first place.
func TestTheRuntimesToolsCanBeRegisteredBeforeItExists(t *testing.T) {
	later := &theRuntime{}

	var (
		_ builtin.Planner     = later
		_ builtin.Activations = later
		_ builtin.Delegator   = later
	)

	// And every method forwards to the same runtime, so filling the handle in
	// once is enough. A field added to theRuntime and left unset would be a
	// tool that silently does nothing.
	value := reflect.ValueOf(later).Elem()
	if value.NumField() != 1 {
		t.Fatalf("theRuntime has %d fields; every one of them needs filling in "+
			"where `later.is` is set", value.NumField())
	}
	if value.Type().Field(0).Type != reflect.TypeOf((*runtime.Runtime)(nil)) {
		t.Fatalf("theRuntime holds something other than the runtime: %s",
			value.Type().Field(0).Type)
	}
}
