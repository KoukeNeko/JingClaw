package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/web"
)

type stubSearcher struct {
	results []web.SearchResult
	err     error
	asked   []web.SearchRequest
}

func (s *stubSearcher) Describe() string { return "stub" }

func (s *stubSearcher) Search(
	_ context.Context,
	request web.SearchRequest,
) ([]web.SearchResult, error) {
	s.asked = append(s.asked, request)
	return s.results, s.err
}

func search(t *testing.T, tool_ *builtin.WebSearch, args string) (tool.Result, error) {
	t.Helper()
	return tool_.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: "web_search", Arguments: json.RawMessage(args),
	})
}

// What the model reads has to carry the address, or it cannot follow it up.
func TestResultsCarryTheAddressToRead(t *testing.T) {
	backend := &stubSearcher{results: []web.SearchResult{
		{Title: "os.Rename", URL: "https://pkg.go.dev/os#Rename", Snippet: "Rename renames a file."},
	}}
	searcher := &builtin.WebSearch{Searcher: backend}

	result, err := search(t, searcher, `{"query": "go rename"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, want := range []string{"os.Rename", "https://pkg.go.dev/os#Rename", "renames a file"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("the result does not carry %q:\n%s", want, result.Content)
		}
	}
}

// Snippets are written by whoever runs the site and by the search service.
// The whole trust story for the web depends on the model knowing that what it
// is reading is a claim rather than a fact.
func TestResultsSayWhereTheyCameFrom(t *testing.T) {
	backend := &stubSearcher{results: []web.SearchResult{
		{Title: "anything", URL: "https://example.com"},
	}}

	result, err := search(t, &builtin.WebSearch{Searcher: backend}, `{"query": "anything"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(result.Content, "not established facts") {
		t.Errorf("the provenance is not stated:\n%s", result.Content)
	}
}

// "There are no results" and "I could not look" lead somewhere completely
// different. A model told the first when the second is true will confidently
// report that something does not exist.
func TestBeingUnableToSearchIsNotFindingNothing(t *testing.T) {
	unavailable := &builtin.WebSearch{Searcher: &stubSearcher{err: web.ErrSearchUnavailable}}
	_, err := search(t, unavailable, `{"query": "anything"}`)
	if err == nil {
		t.Fatal("a search that could not run reported success")
	}

	var refusal *tool.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("the failure is %T", err)
	}
	if !strings.Contains(refusal.SuggestedAction, "could not search") {
		t.Errorf("the model is not told to say it could not look: %q", refusal.SuggestedAction)
	}

	empty := &builtin.WebSearch{Searcher: &stubSearcher{}}
	found, err := search(t, empty, `{"query": "anything"}`)
	if err != nil {
		t.Fatalf("an empty result set was reported as an error: %v", err)
	}
	if !strings.Contains(found.Content, "The search ran") {
		t.Errorf("finding nothing does not say the search ran:\n%s", found.Content)
	}
}

// A deployment without a backend must say so plainly, rather than the model
// discovering it by a call that returns nothing.
func TestNoBackendSaysSoRatherThanFindingNothing(t *testing.T) {
	_, err := search(t, &builtin.WebSearch{}, `{"query": "anything"}`)
	if err == nil {
		t.Fatal("searching with no backend reported success")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("the reason is unclear: %v", err)
	}
}

// The bound is on this side, so a backend that ignores it cannot fill a
// context window.
func TestTheResultCountIsBounded(t *testing.T) {
	backend := &stubSearcher{}
	searcher := &builtin.WebSearch{Searcher: backend}

	if _, err := search(t, searcher, `{"query": "anything", "max_results": 500}`); err != nil {
		t.Fatalf("search: %v", err)
	}
	if backend.asked[0].MaxResults > 20 {
		t.Errorf("asked the backend for %d results", backend.asked[0].MaxResults)
	}
}

// A snippet that runs to a page is not a snippet.
func TestALongSnippetIsCut(t *testing.T) {
	backend := &stubSearcher{results: []web.SearchResult{
		{Title: "long", URL: "https://example.com", Snippet: strings.Repeat("word ", 400)},
	}}

	result, err := search(t, &builtin.WebSearch{Searcher: backend}, `{"query": "anything"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len([]rune(result.Content)) > 2000 {
		t.Errorf("one result produced %d characters", len([]rune(result.Content)))
	}
}
