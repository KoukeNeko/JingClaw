package control

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	// Registers every service the protos define.
	_ "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
)

// The table of what each credential may reach is written out on purpose and is
// not derived from the protos, so that a new service is reachable by nobody
// until somebody decides. This is what keeps the table honest the other way
// round: the operator's own credential is documented as reaching everything, so
// a service the protos define that it cannot reach is one nobody got to, and an
// entry for a service the protos do not define is a name mistyped, which no
// request will ever match.
func TestTheTableOfWhatACredentialMayReachNamesEveryServiceTheProtosDefine(t *testing.T) {
	defined := map[string]bool{}
	protoregistry.GlobalFiles.RangeFilesByPackage("jingclaw.control.v1", func(file protoreflect.FileDescriptor) bool {
		for i := range file.Services().Len() {
			defined[string(file.Services().Get(i).FullName())] = true
		}
		return true
	})
	if len(defined) == 0 {
		t.Fatal("the protos define no services, so there is nothing to hold the table to")
	}

	for name := range defined {
		if !servicesForScope[ScopeControl][name] {
			t.Errorf("%s is defined and the control credential cannot reach it; add it to servicesForScope, "+
				"or take it out of the protos", name)
		}
	}
	for scope, services := range servicesForScope {
		for name := range services {
			if !defined[name] {
				t.Errorf("servicesForScope[%s] has %q, which no proto defines", scope, name)
			}
		}
	}
}
