package web

import (
	"context"
	"errors"
	"time"
)

// Searching is a separate capability from reading.
//
// web_read fetches an address somebody named. Searching chooses the address
// itself, from a query the model wrote, which is a different thing to be
// trusted with: the agent decides where to go rather than being sent. What
// comes back is still somebody else's text, and everything downstream treats
// it that way.

// SearchRequest is one query.
type SearchRequest struct {
	Query string

	// MaxResults bounds the list. Zero uses the backend's default.
	MaxResults int

	// Domains narrows the search to particular sites, where the backend
	// supports it. A backend that cannot must say so rather than silently
	// searching the whole web: an answer drawn from anywhere, when the caller
	// asked for two sites, is worse than no answer.
	Domains []string

	// RecencyDays limits results to the recent past, where the backend
	// supports it. Zero means no limit.
	RecencyDays int
}

// SearchResult is one thing a search found.
//
// A title, an address and a snippet — deliberately not the page. Fetching
// every result to summarise it is how a search costs ten page loads and a
// context window; the agent reads the ones it decides are worth reading.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string

	// PublishedAt is when the page says it was published, where the backend
	// reports it. Nil when it does not, which is not the same as "old".
	PublishedAt *time.Time
}

// Searcher finds addresses for a query.
//
// An interface for the same reason Fetcher is one: which search service a
// deployment uses is a deployment question. Every one of them is a paid API
// with its own key, its own quota and its own idea of what a result looks
// like, and the core should not know which.
type Searcher interface {
	Search(ctx context.Context, request SearchRequest) ([]SearchResult, error)

	// Describe names the backend for the operator, e.g. in a startup line.
	Describe() string
}

// ErrSearchUnavailable says the backend cannot answer at all — no key, no
// network, nothing configured.
//
// Distinct from a search that found nothing. "There are no results" and "I
// could not look" lead somewhere completely different, and a model told the
// first when the second is true will confidently report that something does
// not exist.
var ErrSearchUnavailable = errors.New("web: search is not available")
