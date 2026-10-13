package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// The documents the settings are kept in.
const (
	DocumentConfig  = "config"
	DocumentAgents  = "agents"
	DocumentPersona = "persona"
)

// DocumentVersionsKept is how many versions of each document are kept,
// the current one included. Older ones are discarded as new ones are written.
const DocumentVersionsKept = 20

var (
	// ErrDocumentChanged is a write refused because the document is not the
	// one the edit started from.
	//
	// Somebody else saved it after it was read — from another browser tab, or
	// another process on the same database. Writing over that would discard
	// their change without either of them having seen the other's.
	ErrDocumentChanged = errors.New("storage: the document changed after it was read")

	ErrDocumentVersionNotFound = errors.New("storage: document version not found")
)

// Document is one of the daemon's settings as stored: the text exactly as it
// was given, and the facts that let an editor say which version it started
// from.
//
// For a version read back from history, UpdatedAt is when that version was
// saved.
type Document struct {
	Name string
	Text string

	// Hash is the sha256 of Text in hex, and empty when Text is. See
	// DocumentHash.
	Hash string

	// Source is where Text came from: file, env, default or web.
	Source string

	// Version counts writes, from 1. Zero is a document that was never
	// written.
	Version   int64
	UpdatedAt time.Time
}

// DocumentSeed is a document to create if there is none yet.
type DocumentSeed struct {
	Name   string
	Text   string
	Source string
}

// DocumentHash names a document's text, so whoever edits it can say which
// version they started from.
//
// Empty text hashes to the empty string rather than to the hash of nothing,
// so a document that was never written and one that was cleared are the same
// "started from nothing" token. The rest is the same as config.Hash, which
// this package cannot import; both are sha256 in hex.
func DocumentHash(text string) string {
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// CheckDocumentName refuses a name that is not one of the documents, so every
// store gives the same answer to one. The database enforces the same list
// with a constraint; this is what makes the other stores agree with it.
func CheckDocumentName(name string) error {
	switch name {
	case DocumentConfig, DocumentAgents, DocumentPersona:
		return nil
	}
	return fmt.Errorf("storage: %q is not a document (they are config, agents and persona)", name)
}

// DocumentStore keeps the daemon's settings as documents: three named texts,
// each with a history.
//
// A cleared document is an empty row and never a deleted one. A deleted row
// would be indistinguishable from one that was never written, and seeding
// from the environment would bring back what somebody had just removed.
type DocumentStore interface {
	// Document returns the current text. One that was never written is the
	// zero value with only Name set, and no error: absent and empty are the
	// same state to everything that reads it.
	Document(ctx context.Context, name string) (Document, error)

	// PutDocument replaces the text and keeps the old version in the history.
	//
	// expect is the hash the edit started from. It succeeds when that is the
	// current hash, where an absent document and a cleared one both have the
	// hash "", and otherwise fails with ErrDocumentChanged and writes nothing.
	// The comparison and the write are one statement, so it holds across
	// processes sharing the database, not only across goroutines.
	PutDocument(ctx context.Context, name, text, source, expect string, at time.Time) (Document, error)

	// DocumentVersions lists what is kept, newest first, current included.
	DocumentVersions(ctx context.Context, name string) ([]Document, error)

	// DocumentVersion returns one kept version, or ErrDocumentVersionNotFound
	// for one never written or already discarded.
	DocumentVersion(ctx context.Context, name string, version int64) (Document, error)

	// SeedDocuments creates the documents that do not exist yet and returns
	// their names. It never overwrites: one that exists, even one that was
	// cleared, is left as it is.
	//
	// All or nothing. A seed that cannot be written fails the call and leaves
	// every document as it was, so a first start cannot end up with a
	// configuration but not the instructions that go with it.
	SeedDocuments(ctx context.Context, seeds []DocumentSeed, at time.Time) ([]string, error)
}
