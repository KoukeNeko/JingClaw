package storage

import (
	"strings"
	"unicode"
)

// SearchText is text rewritten into the words a memory search matches on.
//
// Scripts written without spaces — Chinese, Japanese — have no words for a
// tokenizer to find, so a run of them would otherwise be one token: a memory
// saying 使用者偏好用繁體中文回覆 is then found by exactly that sentence and by
// nothing shorter, and 繁體中文 misses it. Each such run becomes the
// overlapping pairs of characters in it instead, which is the length of most
// words in both languages, so a query and a memory that share a word share a
// pair. Everything else is left as it was.
//
// The same rewrite is applied to what is stored and to what is asked, which is
// the whole of the contract: two texts match when their rewrites share a word.
func SearchText(text string) string {
	var (
		out strings.Builder
		run []rune
	)

	flush := func() {
		if len(run) == 0 {
			return
		}
		out.WriteByte(' ')
		if len(run) == 1 {
			out.WriteRune(run[0])
		}
		for i := 0; i+1 < len(run); i++ {
			if i > 0 {
				out.WriteByte(' ')
			}
			out.WriteRune(run[i])
			out.WriteRune(run[i+1])
		}
		out.WriteByte(' ')
		run = run[:0]
	}

	for _, r := range text {
		if unspaced(r) {
			run = append(run, r)
			continue
		}
		flush()
		out.WriteRune(r)
	}
	flush()

	return out.String()
}

// SearchTerms is SearchText split into its words.
func SearchTerms(text string) []string {
	return strings.Fields(SearchText(text))
}

func unspaced(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}
