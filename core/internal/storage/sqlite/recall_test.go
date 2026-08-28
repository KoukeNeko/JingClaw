package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
)

// Lexical search fails silently. It does not crash and it does not report an
// error: the agent simply looks as though it forgot, and nobody finds out
// because nobody knew what it should have found.
//
// So this measures it. The corpus is what somebody would actually write down,
// and the queries are how somebody would actually ask for it later — the same
// thing in other words, in the other language, abbreviated.
//
// What is measured is whether the wanted memory comes back FIRST, not whether
// it comes back at all. Merely being in the results flatters the number badly:
// "can I bounce the kubernetes masters" does return the memory about not
// restarting the control plane, but only because both contain the word "the",
// and an irrelevant memory ranks above it. A model reads the list from the top
// and the limit cuts the bottom off, so a hit at rank three is a miss.
//
// The assertion is a floor rather than perfection, and the misses are printed,
// because the point is to know where the gap is before it matters rather than
// to pretend there is not one.
//
// When this floor becomes the reason recall is not good enough, that is the
// evidence for adding embeddings. Not before.

type recallCase struct {
	query   string
	wants   string
	because string
}

func TestRecallOnParaphrase(t *testing.T) {
	store := newRecallStore(t)

	corpus := map[string]string{
		"deploy":    "the deploy script needs sudo",
		"tests":     "tests are run with go test -race ./...",
		"restart":   "the production cluster must not have its control plane restarted",
		"language":  "answer in Traditional Chinese as used in Taiwan",
		"formatter": "run gofmt before every commit",
		"database":  "migrations live in internal/storage/sqlite/migrations",
	}

	for id, text := range corpus {
		remember(t, store, id, text)
	}

	cases := []recallCase{
		// The words are there. Lexical search is good at this.
		{"deploy script", "deploy", "the exact words"},
		{"sudo", "deploy", "one distinctive word"},
		{"gofmt", "formatter", "a term nothing else uses"},
		{"migrations", "database", "a term nothing else uses"},

		// The words are nearly there.
		{"deploying", "deploy", "a different inflection"},
		{"how do I run the tests", "tests", "a question around the words"},

		// The words are not there at all. This is where it fails, and where
		// somebody asking a reasonable question gets nothing.
		{"can I bounce the kubernetes masters", "restart", "the same thing, other words"},
		{"what language should I reply in", "language", "the topic, not the words"},
		{"code style", "formatter", "the category rather than the tool"},
		{"where do schema changes go", "database", "a synonym for migrations"},
		{"用什麼語言回答", "language", "the other language entirely"},
	}

	var missed []recallCase
	for _, c := range cases {
		found, err := store.SearchMemories(context.Background(), c.query, storage.MemoryQuery{})
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}

		if !leads(found, c.wants) {
			missed = append(missed, c)
		}
	}

	recalled := len(cases) - len(missed)
	t.Logf("recalled %d of %d", recalled, len(cases))
	for _, c := range missed {
		t.Logf("  missed: %-38q wanted %-10s (%s)", c.query, c.wants, c.because)
	}

	// The floor is deliberately low. Raising it is a decision about retrieval,
	// and this number is what that decision would be made on.
	const floor = 5
	if recalled < floor {
		t.Errorf("recalled %d of %d, below the floor of %d — retrieval has got worse",
			recalled, len(cases), floor)
	}
}

// The other half: a query must not drag back everything. Recall that returns
// the whole store is not recall, and it fills the context with noise the model
// then has to weigh.
func TestRecallDoesNotReturnEverything(t *testing.T) {
	store := newRecallStore(t)

	for id, text := range map[string]string{
		"one":   "the deploy script needs sudo",
		"two":   "tests are run with go test -race",
		"three": "answer in Traditional Chinese",
	} {
		remember(t, store, id, text)
	}

	found, err := store.SearchMemories(context.Background(), "deploy", storage.MemoryQuery{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("searching for one thing returned %d memories: %+v", len(found), found)
	}
}

func newRecallStore(t *testing.T) *sqlite.Store {
	t.Helper()

	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "recall.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return store
}

func remember(t *testing.T, store *sqlite.Store, id, text string) {
	t.Helper()

	err := store.Remember(context.Background(), domain.Memory{
		ID:            domain.MemoryID("mem_" + id),
		Scope:         domain.ScopeWorkspace,
		ScopeRef:      "/srv/app",
		Activation:    domain.MemoryRetrieval,
		Text:          text,
		Trust:         domain.TrustUser,
		Origin:        domain.RunOrigin{Kind: domain.OriginLocalClient},
		SourceSession: "ses_1",
		SourceSeq:     1,
		ApprovedBy:    "operator",
		CreatedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}, "")
	if err != nil {
		t.Fatalf("remember %s: %v", id, err)
	}
}

// leads reports whether the wanted memory is the first result.
//
// Not whether it is present. A model reads from the top and the limit cuts the
// bottom off, so a hit at rank three is a miss dressed up as a hit.
func leads(memories []domain.Memory, id string) bool {
	return len(memories) > 0 && memories[0].ID == domain.MemoryID("mem_"+id)
}
