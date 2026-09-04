package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	memstore "github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

func newNoted(t *testing.T) *Noted {
	t.Helper()
	return &Noted{Options: Options{
		Store:        memstore.New(),
		WorkspaceRef: "/the/project",
		NewID:        func() string { return "mem" },
		Clock:        func() time.Time { return time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC) },
	}}
}

func noteFor(t *testing.T, noted *Noted, id string, scope domain.MemoryScope, ref, text string, origin domain.RunOrigin, trust domain.TrustLevel) {
	t.Helper()
	err := noted.Store.Remember(context.Background(), domain.Memory{
		ID: domain.MemoryID(id), Scope: scope, ScopeRef: ref,
		Activation: domain.MemoryRetrieval, Text: text,
		Trust: trust, Origin: origin, CreatedAt: noted.Now(), ValidFrom: noted.Now(),
	}, "")
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
}

var aliceAsking = domain.Run{ID: "run_1", SessionID: "ses_1", Origin: alice}

// The notes put in front of a turn are the ones its sender may read: the
// project's and their own. What another account told the agent is never put
// in front of somebody else's turn.
func TestNotesFollowThePersonAsking(t *testing.T) {
	noted := newNoted(t)
	noteFor(t, noted, "a", domain.ScopePrincipal, "discord:111", "Alice indents with tabs", alice, domain.TrustUntrusted)
	noteFor(t, noted, "b", domain.ScopePrincipal, "discord:222", "Bob indents with spaces", bob, domain.TrustUntrusted)
	noteFor(t, noted, "c", domain.ScopeWorkspace, "/the/project", "The project is written in Go", operator, domain.TrustUser)

	label := noted.For(context.Background(), aliceAsking, "how should I indent, and which language is this")

	if !strings.HasPrefix(label, notedHeader) || !strings.HasSuffix(label, "]") {
		t.Errorf("the label is not framed as what it is:\n%s", label)
	}
	for _, want := range []string{"Alice indents with tabs", "The project is written in Go"} {
		if !strings.Contains(label, want) {
			t.Errorf("missing %q:\n%s", want, label)
		}
	}
	if strings.Contains(label, "Bob") {
		t.Errorf("another account's note was put in front of Alice's turn:\n%s", label)
	}
}

// Every note says where it came from — who, when, and whether from outside
// this machine — in the terms the turn line uses.
func TestANoteSaysWhereItCameFrom(t *testing.T) {
	noted := newNoted(t)
	noteFor(t, noted, "a", domain.ScopePrincipal, "discord:111", "Alice indents with tabs", alice, domain.TrustUntrusted)
	noteFor(t, noted, "c", domain.ScopeWorkspace, "/the/project", "The project is written in Go", operator, domain.TrustUser)

	label := noted.For(context.Background(), aliceAsking, "tabs and Go")

	if !strings.Contains(label, "Alice indents with tabs (<@111> on discord, 2026-09-05, from outside this machine)") {
		t.Errorf("a note from discord is not labelled as such:\n%s", label)
	}
	if !strings.Contains(label, "The project is written in Go (this machine, 2026-09-05)") {
		t.Errorf("a note from this machine is not labelled as such:\n%s", label)
	}
}

// A worker, a schedule, and a turn with nothing said get no notes.
func TestNoNotesWhereNobodyIsAsking(t *testing.T) {
	noted := newNoted(t)
	noteFor(t, noted, "a", domain.ScopePrincipal, "discord:111", "Alice indents with tabs", alice, domain.TrustUntrusted)

	cases := map[string]struct {
		run  domain.Run
		said string
	}{
		"a worker":     {domain.Run{Kind: domain.RunWorker, Origin: alice}, "tabs"},
		"a schedule":   {domain.Run{Origin: domain.FromASchedule("sch_1")}, "tabs"},
		"nothing said": {domain.Run{Origin: alice}, "   "},
	}
	for name, tc := range cases {
		if label := noted.For(context.Background(), tc.run, tc.said); label != "" {
			t.Errorf("%s got notes:\n%s", name, label)
		}
	}
}

// The notes are bounded, in number and in bytes, and a bound of nothing that
// fits is no label at all.
func TestNotesAreBounded(t *testing.T) {
	noted := newNoted(t)
	for i, text := range []string{"Alice indents with tabs", "Alice likes tabs in Go", "Alice sets tabs to four"} {
		noteFor(t, noted, string(rune('a'+i)), domain.ScopePrincipal, "discord:111", text, alice, domain.TrustUntrusted)
	}

	noted.Limit = 1
	if label := noted.For(context.Background(), aliceAsking, "tabs"); strings.Count(label, "\n- ") != 1 {
		t.Errorf("a limit of one gave:\n%s", label)
	}

	noted.Limit = 3
	noted.MaxBytes = 10
	if label := noted.For(context.Background(), aliceAsking, "tabs"); label != "" {
		t.Errorf("a bound nothing fits in still gave:\n%s", label)
	}
}
