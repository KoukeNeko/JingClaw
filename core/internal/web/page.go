package web

import (
	"context"
	"strings"
	"unicode"
)

// ExtractorVersion identifies how the text in a Page was derived from the
// document.
//
// It is recorded with every fetch. When a stored observation is read back
// months later and reads oddly, the first question is whether the page changed
// or the reader did, and without this there is no way to tell.
const ExtractorVersion = "visible-text/1"

// Page is what came back from one address.
type Page struct {
	// RequestedURL is what was asked for; FinalURL is where it ended up.
	// A redirect is part of the answer, not an implementation detail: the
	// difference between them is often the whole story.
	RequestedURL string
	FinalURL     string

	Status int
	Title  string

	// Text is the page as a reader would see it, with the markup gone.
	Text string

	// Links are the destinations the page offers, so the agent can say where
	// it would go next instead of guessing a URL.
	Links []Link
}

// Link is one destination named by a page.
type Link struct {
	Text string
	URL  string
}

// Fetcher retrieves a page.
//
// It is an interface because how a page is fetched is a deployment question
// rather than an architectural one. A plain HTTP client and a full browser
// differ in what fraction of the web they can reach, not in what the agent
// does with the result, and nothing above this line should have to know which
// one is installed.
type Fetcher interface {
	// Fetch retrieves one address. The address has already been checked; a
	// Fetcher is responsible for the addresses reached by any redirect.
	Fetch(ctx context.Context, url string) (Page, error)

	// Describe names the backend for the operator, e.g. in a startup line.
	Describe() string
}

// CollapseWhitespace makes extracted text readable in a transcript.
//
// A rendered page is mostly vertical space: layout produces runs of blank
// lines that carry no information and cost context. Runs of them collapse to
// one, and trailing space on a line goes, but single line breaks stay, because
// they are the paragraph structure.
func CollapseWhitespace(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	var (
		out   []string
		blank int
	)
	for _, line := range lines {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, line)
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}
