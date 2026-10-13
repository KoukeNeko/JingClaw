package memory

import (
	"context"
	"slices"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

func (s *Store) Document(ctx context.Context, name string) (storage.Document, error) {
	if err := ctx.Err(); err != nil {
		return storage.Document{}, err
	}
	if err := storage.CheckDocumentName(name); err != nil {
		return storage.Document{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.currentDocument(name), nil
}

// currentDocument is the document as stored, or the zero value with only the
// name set for one never written. The caller holds the lock.
func (s *Store) currentDocument(name string) storage.Document {
	if doc, ok := s.documents[name]; ok {
		return doc
	}
	return storage.Document{Name: name}
}

func (s *Store) PutDocument(
	ctx context.Context, name, text, source, expect string, at time.Time,
) (storage.Document, error) {
	if err := ctx.Err(); err != nil {
		return storage.Document{}, err
	}
	if err := storage.CheckDocumentName(name); err != nil {
		return storage.Document{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// An absent document has hash "" like a cleared one, which is what lets
	// expect "" mean "I started from nothing" for both.
	current := s.currentDocument(name)
	if current.Hash != expect {
		return storage.Document{}, storage.ErrDocumentChanged
	}

	doc := storage.Document{
		Name: name, Text: text, Hash: storage.DocumentHash(text),
		Source: source, Version: current.Version + 1, UpdatedAt: at.UTC(),
	}
	s.keepDocument(doc)
	return doc, nil
}

// keepDocument makes doc the current version and adds it to the history,
// discarding the oldest past DocumentVersionsKept. The caller holds the lock.
func (s *Store) keepDocument(doc storage.Document) {
	s.documents[doc.Name] = doc

	history := append(s.documentVersions[doc.Name], doc)
	if extra := len(history) - storage.DocumentVersionsKept; extra > 0 {
		// A copy, so the discarded texts are not kept alive by the array
		// underneath.
		history = slices.Clone(history[extra:])
	}
	s.documentVersions[doc.Name] = history
}

func (s *Store) DocumentVersions(ctx context.Context, name string) ([]storage.Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := storage.CheckDocumentName(name); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	versions := slices.Clone(s.documentVersions[name])
	slices.Reverse(versions)
	return versions, nil
}

func (s *Store) DocumentVersion(
	ctx context.Context, name string, version int64,
) (storage.Document, error) {
	if err := ctx.Err(); err != nil {
		return storage.Document{}, err
	}
	if err := storage.CheckDocumentName(name); err != nil {
		return storage.Document{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, doc := range s.documentVersions[name] {
		if doc.Version == version {
			return doc, nil
		}
	}
	return storage.Document{}, storage.ErrDocumentVersionNotFound
}

func (s *Store) SeedDocuments(
	ctx context.Context, seeds []storage.DocumentSeed, at time.Time,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Every name is checked before anything is written, which is what makes
	// this all or nothing here: the database gets it from a transaction.
	for _, seed := range seeds {
		if err := storage.CheckDocumentName(seed.Name); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var written []string
	for _, seed := range seeds {
		if _, exists := s.documents[seed.Name]; exists {
			continue
		}
		s.keepDocument(storage.Document{
			Name: seed.Name, Text: seed.Text, Hash: storage.DocumentHash(seed.Text),
			Source: seed.Source, Version: 1, UpdatedAt: at.UTC(),
		})
		written = append(written, seed.Name)
	}
	return written, nil
}
