package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Brave searches through Brave's API.
//
// One backend rather than several. Every search service has its own key, its
// own quota and its own result shape, and writing four adapters from four sets
// of documentation is four things that have never run — this project has been
// caught by exactly that before. One, behind an interface, and the next is
// added when somebody has a key to try it with.
//
// Brave because its API is a plain documented GET returning JSON, with a free
// tier: the least machinery between a query and a result.
type Brave struct {
	// Key is the subscription token. Never logged.
	Key string

	// Endpoint is the API root. Configurable so a check can point it
	// somewhere it controls, which is the difference between testing this
	// adapter and testing the internet.
	Endpoint string

	// HTTP is the client. Left nil, one with a sensible timeout is used.
	HTTP *http.Client
}

const (
	braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

	// braveMaxResults is what the API accepts in one call.
	braveMaxResults = 20

	defaultSearchResults = 8
	searchTimeout        = 20 * time.Second
)

func (b *Brave) Describe() string { return "brave" }

func (b *Brave) Search(ctx context.Context, request SearchRequest) ([]SearchResult, error) {
	if b.Key == "" {
		return nil, fmt.Errorf("%w: no Brave subscription token is configured", ErrSearchUnavailable)
	}
	if strings.TrimSpace(request.Query) == "" {
		return nil, fmt.Errorf("web: the query is empty")
	}

	count := request.MaxResults
	if count <= 0 {
		count = defaultSearchResults
	}
	if count > braveMaxResults {
		count = braveMaxResults
	}

	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = braveEndpoint
	}

	query := url.Values{}
	query.Set("q", buildQuery(request))
	query.Set("count", strconv.Itoa(count))
	if request.RecencyDays > 0 {
		query.Set("freshness", freshnessFor(request.RecencyDays))
	}

	callCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.Key)

	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: searchTimeout}
	}

	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()

	// Bounded: this is somebody else's server, and an unbounded read lets it
	// decide how much memory this process uses.
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		// The key is in a header, so an error must not carry the request.
		// What it does carry is enough to tell a quota from a bad key, which
		// are different problems for whoever has to fix them.
		return nil, fmt.Errorf("%w: the search service answered %d (%s)",
			ErrSearchUnavailable, response.StatusCode, http.StatusText(response.StatusCode))
	}

	return parseBrave(raw)
}

// buildQuery folds a domain filter into the query itself.
//
// Brave has no separate parameter for it, and "site:" is what the query
// syntax offers. Done here rather than left to the model, so that a caller
// asking for two sites gets two sites whichever backend is configured.
func buildQuery(request SearchRequest) string {
	if len(request.Domains) == 0 {
		return request.Query
	}

	sites := make([]string, 0, len(request.Domains))
	for _, domain := range request.Domains {
		domain = strings.TrimSpace(domain)
		if domain != "" {
			sites = append(sites, "site:"+domain)
		}
	}
	if len(sites) == 0 {
		return request.Query
	}
	return request.Query + " (" + strings.Join(sites, " OR ") + ")"
}

// freshnessFor maps days onto what the API accepts, which is a set of named
// windows rather than a number.
func freshnessFor(days int) string {
	switch {
	case days <= 1:
		return "pd"
	case days <= 7:
		return "pw"
	case days <= 31:
		return "pm"
	default:
		return "py"
	}
}

func parseBrave(raw []byte) ([]SearchResult, error) {
	var answer struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				PageAge     string `json:"page_age"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("web: the search service answered unreadably: %w", err)
	}

	results := make([]SearchResult, 0, len(answer.Web.Results))
	for _, one := range answer.Web.Results {
		if one.URL == "" {
			continue
		}
		results = append(results, SearchResult{
			Title: stripTags(one.Title),
			URL:   one.URL,
			// The description carries <strong> around the matched words.
			// Removed rather than kept: the model reads text, and markup in
			// the middle of a sentence is noise it has to ignore.
			Snippet:     stripTags(one.Description),
			PublishedAt: parseAge(one.PageAge),
		})
	}
	return results, nil
}

// stripTags removes the markup a snippet arrives with.
func stripTags(text string) string {
	var out strings.Builder
	inTag := false

	for _, r := range text {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(out.String())
}

// parseAge reads a published date, where there is one.
//
// Nil rather than a zero time when there is not: "no date" and "1 January
// year zero" are different, and only one of them would be drawn as old.
func parseAge(age string) *time.Time {
	if age == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if at, err := time.Parse(layout, age); err == nil {
			return &at
		}
	}
	return nil
}
