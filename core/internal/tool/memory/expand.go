package memory

import (
	"context"
	"strings"
	"time"
	"unicode"
)

const (
	// expandBelow is how few results make a search worth broadening.
	//
	// One: only a search that found nothing at all. A search that found
	// something has already answered the question it was asked, and the
	// failure this exists for is the silent one — the words did not overlap,
	// so nothing came back and nothing said anything was missed.
	//
	// Broadening on a partial result would fire on nearly every search, since
	// a small corpus rarely fills the limit, and would spend a model call
	// each time to append worse matches below better ones.
	expandBelow = 1

	// maxExpansionTerms bounds what an expander may add to a search.
	//
	// A model asked for other phrasings will happily produce thirty. Past a
	// handful they stop being other words for the same thing and start being
	// other things, and since every term is ORed into one query, each one
	// past that point is a way to match a memory about something else.
	maxExpansionTerms = 8

	// maxExpansionTermLen rejects a "term" that is really a sentence. An
	// expander that starts explaining itself should not have its prose
	// searched for.
	maxExpansionTermLen = 40

	defaultExpandTimeout = 15 * time.Second
)

// Expander proposes other words for what a search was looking for.
//
// It exists because the memory index matches words, and the thing that makes
// that fail is not a hard question: "don't build a second component that
// already exists" and "should I add a new modal?" are the same subject with no
// word in common. The index cannot see that, returns nothing, and says nothing
// about what it did not look for.
//
// What comes back is used as search terms and as nothing else. It is never
// read as instructions, and it reaches the index through the same quoting as
// any model-written query, so it cannot be a way to write MATCH syntax or to
// widen the scopes a turn is allowed to see.
type Expander interface {
	// Expand returns other words the same subject might have been written in.
	// An expander that has nothing to add returns none, which is not an error.
	Expand(ctx context.Context, query string) ([]string, error)
}

// broaden asks the expander for other phrasings and returns the query to
// search for instead.
//
// The original words are kept and the alternatives added to them. Every term
// is ORed, so the broadened query matches everything the original did: the
// caller can use this result in place of the first one rather than merging
// two, and the ranking still sees the words that were actually asked for.
//
// The second return says whether anything was added, because a search the
// agent did not ask to broaden has to be reported as broadened. A recall that
// quietly answers a different question than the one it was given is the same
// failure this is here to fix, pointed the other way.
func (o Options) broaden(ctx context.Context, query string) (string, bool) {
	if o.Expander == nil || strings.TrimSpace(query) == "" {
		return query, false
	}

	timeout := o.ExpandTimeout
	if timeout <= 0 {
		timeout = defaultExpandTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	terms, err := o.Expander.Expand(ctx, query)
	if err != nil {
		// A search that could not be broadened is still a search. It found
		// nothing, which is what it would have reported anyway, so this is
		// logged and the answer stands rather than turning a miss into a
		// tool failure the model has to reason about.
		o.Logger().Warn("could not broaden a memory search",
			"error", err, "query", query)
		return query, false
	}

	added := usableTerms(terms, query)
	if len(added) == 0 {
		return query, false
	}

	return query + " " + strings.Join(added, " "), true
}

// usableTerms keeps what can honestly be searched for.
//
// An expander is a language model, and its answer arrives as text however
// firmly it was asked for a list. Numbering, bullets, quotes and trailing
// commentary all turn up. Anything left that is punctuation, too long to be a
// word, or already in the query is dropped rather than searched for.
func usableTerms(terms []string, query string) []string {
	seen := make(map[string]bool)
	for _, word := range strings.Fields(strings.ToLower(query)) {
		seen[word] = true
	}

	var kept []string

	for _, term := range terms {
		for _, word := range strings.Fields(term) {
			word = strings.TrimFunc(word, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsNumber(r)
			})
			if word == "" || len([]rune(word)) > maxExpansionTermLen {
				continue
			}

			folded := strings.ToLower(word)
			if seen[folded] {
				continue
			}
			seen[folded] = true

			kept = append(kept, word)
			if len(kept) == maxExpansionTerms {
				return kept
			}
		}
	}

	return kept
}
