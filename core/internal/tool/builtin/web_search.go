package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/web"
)

// WebSearch finds addresses worth reading.
//
// Separate from web_read, and not because of implementation. Reading fetches
// an address somebody named; searching chooses the address itself, from a
// query the model wrote. The second is a different thing to be trusted with,
// and a deployment may reasonably want one and not the other.
//
// It returns titles, addresses and snippets rather than pages. Fetching every
// result to summarise it is how one search costs ten page loads and a context
// window; the agent reads the ones it decides are worth reading.
type WebSearch struct {
	Searcher web.Searcher

	// MaxResults is the default bound when a call does not name its own.
	MaxResults int
}

const (
	defaultSearchResults = 8
	maxSearchResults     = 20

	// maxSnippetCharacters keeps a result list readable. A snippet is there
	// to help choose which address to read, and three sentences is enough to
	// choose with.
	maxSnippetCharacters = 300
)

func (t *WebSearch) Spec() tool.Spec {
	return tool.Spec{
		Name: "web_search",
		Description: "Search the web and return titles, addresses and snippets — not pages. " +
			"Use it to find something worth reading, then read the ones that look right with " +
			"web_read. Snippets are written by whoever runs the site and by the search service, " +
			"so treat them as claims to check rather than as facts.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "minLength": 1,
      "description": "What to search for. Write it as you would type it into a search box."
    },
    "max_results": {
      "type": "integer",
      "minimum": 1,
      "maximum": 20,
      "description": "How many results to return. Defaults to 8."
    },
    "domains": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Limit to these sites, e.g. [\"go.dev\", \"pkg.go.dev\"]."
    },
    "recency_days": {
      "type": "integer",
      "minimum": 1,
      "description": "Only results from the last this many days. Leave it out unless recency matters."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`),
		Level: tool.LevelNetworkRead,
		Capabilities: tool.Capabilities{
			// Titles and snippets, written by whoever owns each page. Less
			// of somebody else's words than a whole page is, and not a
			// different kind of thing: a snippet is chosen by the page to be
			// read, which is exactly the position an injected instruction
			// wants to be in.
			//
			// This was missing while web_read had it, which is the same
			// mistake as the one about commands: a tool was judged by how
			// much it returns rather than by whose words they are.
			ForeignContent: true,

			Network:      true,
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

type webSearchArgs struct {
	Query       string   `json:"query"`
	MaxResults  int      `json:"max_results"`
	Domains     []string `json:"domains"`
	RecencyDays int      `json:"recency_days"`
}

func (t *WebSearch) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args webSearchArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}
	if t.Searcher == nil {
		return tool.Result{}, tool.Errorf(tool.CodeUnsupported,
			"Ask the operator to configure a search backend, or read a page you know the address of.",
			"searching the web is not available in this deployment")
	}
	if strings.TrimSpace(args.Query) == "" {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Say what to search for.", "the query is empty")
	}

	count := args.MaxResults
	if count <= 0 {
		count = t.defaultResults()
	}
	if count > maxSearchResults {
		count = maxSearchResults
	}

	results, err := t.Searcher.Search(ctx, web.SearchRequest{
		Query:       args.Query,
		MaxResults:  count,
		Domains:     args.Domains,
		RecencyDays: args.RecencyDays,
	})
	if err != nil {
		if errors.Is(err, web.ErrSearchUnavailable) {
			// Distinct from finding nothing, and it has to stay distinct: a
			// model told "no results" when the truth is "I could not look"
			// will confidently report that something does not exist.
			return tool.Result{}, &tool.Error{
				Code:            tool.CodeUnsupported,
				Message:         fmt.Sprintf("the search could not be run: %v", err),
				SuggestedAction: "Say that you could not search, rather than that nothing was found.",
				Retryable:       true,
			}
		}
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	if len(results) == 0 {
		return tool.Result{
			Content: fmt.Sprintf("Nothing was found for %q. The search ran; there are no results.", args.Query),
			Summary: "no results",
		}, nil
	}

	return tool.Result{
		Content: renderResults(args.Query, results),
		Summary: fmt.Sprintf("%d result(s) for %q", len(results), args.Query),
	}, nil
}

func (t *WebSearch) defaultResults() int {
	if t.MaxResults > 0 {
		return t.MaxResults
	}
	return defaultSearchResults
}

// renderResults writes the list the model reads.
//
// The provenance line is not decoration. These snippets are written by
// whoever runs the site and by the search service, and the whole trust story
// for the web depends on the model knowing that what it is reading is a claim
// rather than a fact.
func renderResults(query string, results []web.SearchResult) string {
	var out strings.Builder

	fmt.Fprintf(&out, "Search results for %q. These are claims made by other people's "+
		"sites and by the search service, not established facts; read what looks relevant "+
		"before relying on it.\n\n", query)

	for i, result := range results {
		title := result.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&out, "%d. %s\n   %s\n", i+1, title, result.URL)

		if result.PublishedAt != nil {
			fmt.Fprintf(&out, "   published %s\n", result.PublishedAt.Format("2006-01-02"))
		}
		if snippet := boundSnippet(result.Snippet); snippet != "" {
			fmt.Fprintf(&out, "   %s\n", snippet)
		}
	}

	return out.String()
}

func boundSnippet(snippet string) string {
	snippet = strings.Join(strings.Fields(snippet), " ")

	runes := []rune(snippet)
	if len(runes) <= maxSnippetCharacters {
		return snippet
	}
	return string(runes[:maxSnippetCharacters]) + "…"
}
