package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/web"
)

// These run against a stub that answers whatever it is asked, so a wrong
// parameter name or a field this reads from the wrong place would pass. The
// shapes are taken from Brave's documented response; this has never been near
// the real API, which needs a subscription token.
//
// What they do check is everything on this side of the wire: the request that
// would go out, and what is made of an answer in the documented shape.

type braveStub struct {
	body    string
	status  int
	queries []string
	tokens  []string
}

func (s *braveStub) serve(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.queries = append(s.queries, r.URL.RawQuery)
		s.tokens = append(s.tokens, r.Header.Get("X-Subscription-Token"))

		if s.status != 0 && s.status != http.StatusOK {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(`{"error":"no"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(server.Close)
	return server
}

const twoResults = `{
  "web": {
    "results": [
      {"title": "Atomic file <strong>replacement</strong> on Windows",
       "url": "https://learn.microsoft.com/one",
       "description": "Use <strong>ReplaceFile</strong> to swap a file.",
       "page_age": "2024-03-01T00:00:00"},
      {"title": "os.Rename", "url": "https://pkg.go.dev/os#Rename",
       "description": "Rename renames a file."}
    ]
  }
}`

func newBrave(t *testing.T, stub *braveStub) *web.Brave {
	t.Helper()
	return &web.Brave{Key: "not-a-real-token", Endpoint: stub.serve(t).URL}
}

// The results are what a caller acts on, so every field has to survive.
func TestASearchReturnsWhatItFound(t *testing.T) {
	stub := &braveStub{body: twoResults}
	results, err := newBrave(t, stub).Search(context.Background(),
		web.SearchRequest{Query: "atomic file replacement"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].URL != "https://learn.microsoft.com/one" {
		t.Errorf("the address is %q", results[0].URL)
	}
	if results[0].PublishedAt == nil {
		t.Error("a result with a date came back without one")
	}
	if results[1].PublishedAt != nil {
		t.Error("a result with no date was given one")
	}
}

// A snippet arrives with markup around the matched words. The model reads
// text, and tags in the middle of a sentence are noise it has to ignore.
func TestMarkupIsStrippedFromTitlesAndSnippets(t *testing.T) {
	stub := &braveStub{body: twoResults}
	results, err := newBrave(t, stub).Search(context.Background(),
		web.SearchRequest{Query: "anything"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	for _, result := range results {
		if strings.ContainsAny(result.Title+result.Snippet, "<>") {
			t.Errorf("markup survived: %q / %q", result.Title, result.Snippet)
		}
	}
	if !strings.Contains(results[0].Snippet, "ReplaceFile") {
		t.Errorf("stripping the markup removed the words too: %q", results[0].Snippet)
	}
}

// A caller that asked for two sites must get two sites. The service has no
// parameter for it, so it goes into the query — done here rather than left to
// the model, so the answer is the same whichever backend is configured.
func TestADomainFilterReachesTheQuery(t *testing.T) {
	stub := &braveStub{body: twoResults}
	if _, err := newBrave(t, stub).Search(context.Background(), web.SearchRequest{
		Query:   "rename",
		Domains: []string{"go.dev", "pkg.go.dev"},
	}); err != nil {
		t.Fatalf("search: %v", err)
	}

	sent := stub.queries[0]
	for _, want := range []string{"site%3Ago.dev", "site%3Apkg.go.dev"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the request does not narrow to the sites asked for: %s", sent)
		}
	}
}

// Recency is a set of named windows rather than a number of days.
func TestRecencyBecomesAWindowTheServiceUnderstands(t *testing.T) {
	for days, want := range map[int]string{1: "pd", 5: "pw", 20: "pm", 300: "py"} {
		stub := &braveStub{body: twoResults}
		if _, err := newBrave(t, stub).Search(context.Background(), web.SearchRequest{
			Query: "anything", RecencyDays: days,
		}); err != nil {
			t.Fatalf("search: %v", err)
		}
		if !strings.Contains(stub.queries[0], "freshness="+want) {
			t.Errorf("%d days became %q, want %q", days, stub.queries[0], want)
		}
	}
}

// The token goes in a header, never in the address: a URL ends up in logs and
// in error messages.
func TestTheTokenNeverReachesTheAddress(t *testing.T) {
	stub := &braveStub{body: twoResults}
	if _, err := newBrave(t, stub).Search(context.Background(),
		web.SearchRequest{Query: "anything"}); err != nil {
		t.Fatalf("search: %v", err)
	}

	if strings.Contains(stub.queries[0], "not-a-real-token") {
		t.Errorf("the token is in the query string: %s", stub.queries[0])
	}
	if stub.tokens[0] != "not-a-real-token" {
		t.Errorf("the token did not reach the header: %q", stub.tokens[0])
	}
}

// "I could not look" and "there is nothing" lead somewhere completely
// different, and a model told the first when the second is true will
// confidently report that something does not exist.
func TestBeingUnableToSearchIsItsOwnFailure(t *testing.T) {
	unconfigured := &web.Brave{}
	if _, err := unconfigured.Search(context.Background(),
		web.SearchRequest{Query: "anything"}); !errors.Is(err, web.ErrSearchUnavailable) {
		t.Errorf("a missing key is reported as %v", err)
	}

	stub := &braveStub{status: http.StatusTooManyRequests}
	_, err := newBrave(t, stub).Search(context.Background(), web.SearchRequest{Query: "anything"})
	if !errors.Is(err, web.ErrSearchUnavailable) {
		t.Errorf("a refused request is reported as %v", err)
	}
	if strings.Contains(err.Error(), "not-a-real-token") {
		t.Errorf("the error carries the token: %v", err)
	}
}

// A search that genuinely found nothing is not a failure.
func TestFindingNothingIsNotAnError(t *testing.T) {
	stub := &braveStub{body: `{"web":{"results":[]}}`}
	results, err := newBrave(t, stub).Search(context.Background(),
		web.SearchRequest{Query: "a phrase nobody has written"})
	if err != nil {
		t.Fatalf("an empty result set was reported as an error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results", len(results))
	}
}

// A result with no address cannot be read, so it is not offered.
func TestAResultWithNoAddressIsDropped(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"web": map[string]any{"results": []map[string]any{
			{"title": "no link", "description": "nothing to read"},
			{"title": "fine", "url": "https://example.com", "description": "something"},
		}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	stub := &braveStub{body: string(body)}
	results, searchErr := newBrave(t, stub).Search(context.Background(),
		web.SearchRequest{Query: "anything"})
	if searchErr != nil {
		t.Fatalf("search: %v", searchErr)
	}
	if len(results) != 1 || results[0].URL != "https://example.com" {
		t.Errorf("a result with no address was kept: %+v", results)
	}
}
