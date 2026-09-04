package gateway

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// The tools a worker ran on the parent's behalf show in the parent's footer.
//
// A worker's steps are hidden from the conversation, and rightly; but what a
// person reading the footer wants to know is which tools did the work, and
// "investigate" is not an answer to that when the answer is an MCP tool or a
// skill the worker used.
func TestAWorkersToolsCountTowardsItsParent(t *testing.T) {
	projector := &Projector{records: make(map[domain.RunID]*runRecord)}

	worker := domain.Run{ID: "run_worker", Kind: domain.RunWorker, ParentRunID: "run_parent"}
	for _, record := range projector.recordsFor(worker) {
		record.requested(domain.ToolCallRequested{CallID: "c1", Name: "mcp_zhtw_zhtw", Arguments: `{"text":"x"}`})
		record.completed(domain.ToolCallCompleted{CallID: "c1", Name: "mcp_zhtw_zhtw"}, 5)
	}

	parent := projector.record("run_parent").summarise("p", "m")
	if len(parent.Tools) != 1 || parent.Tools[0].Name != "mcp_zhtw_zhtw" || parent.Tools[0].Calls != 1 {
		t.Errorf("the parent's footer does not list the worker's tool once: %+v", parent.Tools)
	}

	// Counted in both, not moved.
	own := projector.record("run_worker").summarise("p", "m")
	if len(own.Tools) != 1 {
		t.Errorf("the worker's own record lost its tool: %+v", own.Tools)
	}
}

// A run that is nobody's worker counts only towards itself, and a worker
// with no parent recorded has nowhere else to count.
func TestAPlainRunCountsOnlyTowardsItself(t *testing.T) {
	projector := &Projector{records: make(map[domain.RunID]*runRecord)}

	if got := len(projector.recordsFor(domain.Run{ID: "run_1"})); got != 1 {
		t.Errorf("a plain run has %d records, want 1", got)
	}
	if got := len(projector.recordsFor(domain.Run{ID: "run_2", Kind: domain.RunWorker})); got != 1 {
		t.Errorf("a worker with no parent has %d records, want 1", got)
	}
	if len(projector.records) != 2 {
		t.Errorf("records were created for runs that do not exist: %v", len(projector.records))
	}
}
