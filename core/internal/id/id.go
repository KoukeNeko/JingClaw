// Package id generates identifiers for sessions, runs, messages and events.
//
// ULIDs are used because they sort lexicographically by creation time, which
// makes logs and database scans readable in chronological order without a
// separate timestamp index.
package id

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a bare ULID.
func New() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// WithPrefix returns an identifier such as "ses_01K…", so an ID pasted into a
// bug report says what kind of thing it names.
func WithPrefix(prefix string) string {
	var b strings.Builder
	b.Grow(len(prefix) + 1 + ulid.EncodedSize)
	b.WriteString(prefix)
	b.WriteByte('_')
	b.WriteString(New())
	return b.String()
}
