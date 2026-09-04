package tool_test

import (
	"context"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// leveledTool is a tool that offers its own level for a call, for testing what
// EffectiveLevel does with the offer.
type leveledTool struct {
	floor   tool.Level
	forCall tool.Level
}

func (t leveledTool) Spec() tool.Spec { return tool.Spec{Name: "leveled", Level: t.floor} }

func (t leveledTool) Execute(context.Context, tool.Call) (tool.Result, error) {
	return tool.Result{}, nil
}

func (t leveledTool) LevelFor(tool.Call) tool.Level { return t.forCall }

// EffectiveLevel takes the higher of the two and never the lower.
//
// The load-bearing rule for everything above it: a tool cannot lower the level
// it is judged at, so nothing a tool — or a skill whose instructions a tool
// might carry — says about itself can make a call cheaper to permit than its
// floor. It may only ask to be treated as more dangerous.
func TestEffectiveLevelMayOnlyRaise(t *testing.T) {
	floor := tool.LevelExecute

	// A tool asking to be treated as less dangerous is ignored.
	lower := leveledTool{floor: floor, forCall: tool.LevelInternal}
	if got := tool.EffectiveLevel(lower, tool.Call{}); got != floor {
		t.Errorf("a tool lowered its own level: judged at %s, floor is %s", got, floor)
	}

	// A tool asking to be treated as more dangerous is honoured.
	higher := leveledTool{floor: floor, forCall: tool.LevelHighImpact}
	if got := tool.EffectiveLevel(higher, tool.Call{}); got != tool.LevelHighImpact {
		t.Errorf("a tool could not raise its own level: judged at %s", got)
	}

	// A tool that offers nothing is judged at its floor.
	plain := leveledTool{floor: floor, forCall: floor}
	if got := tool.EffectiveLevel(plain, tool.Call{}); got != floor {
		t.Errorf("a plain call was not judged at its floor: %s", got)
	}
}
