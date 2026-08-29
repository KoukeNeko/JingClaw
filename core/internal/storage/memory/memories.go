package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// The in-memory store carries memories so that tests which do not want a
// database still exercise the same behaviour. Search is a substring match
// rather than a real index: the point of this implementation is to be obvious,
// and the conformance suite is what proves the two agree where it matters.

func (s *Store) Remember(
	_ context.Context,
	memory domain.Memory,
	supersedes domain.MemoryID,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if supersedes != "" {
		existing, ok := s.memories[supersedes]
		if !ok || existing.InvalidatedAt != nil {
			return fmt.Errorf("memory: %s is not a memory that can be superseded", supersedes)
		}

		at := memory.CreatedAt
		existing.InvalidatedAt = &at
		existing.SupersededBy = memory.ID
		s.memories[supersedes] = existing
	}

	s.memories[memory.ID] = memory
	s.memoryOrder = append(s.memoryOrder, memory.ID)
	return nil
}

func (s *Store) Memories(_ context.Context, query storage.MemoryQuery) ([]domain.Memory, error) {
	return s.selectMemories(query, func(domain.Memory) bool { return true }), nil
}

func (s *Store) SearchMemories(
	_ context.Context,
	text string,
	query storage.MemoryQuery,
) ([]domain.Memory, error) {
	terms := strings.Fields(strings.ToLower(text))
	if len(terms) == 0 {
		return nil, nil
	}

	return s.selectMemories(query, func(candidate domain.Memory) bool {
		body := strings.ToLower(candidate.Text)
		return slices.ContainsFunc(terms, func(term string) bool {
			return strings.Contains(body, term)
		})
	}), nil
}

func (s *Store) Memory(_ context.Context, id domain.MemoryID) (domain.Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	found, ok := s.memories[id]
	if !ok {
		return domain.Memory{}, storage.ErrMemoryNotFound
	}
	return found, nil
}

func (s *Store) Forget(_ context.Context, id domain.MemoryID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.memories[id]; !ok {
		return storage.ErrMemoryNotFound
	}

	// Anything pointing at it would otherwise refer to something gone.
	for other, candidate := range s.memories {
		if candidate.SupersededBy == id {
			candidate.SupersededBy = ""
			s.memories[other] = candidate
		}
	}

	delete(s.memories, id)
	s.memoryOrder = slices.DeleteFunc(s.memoryOrder,
		func(candidate domain.MemoryID) bool { return candidate == id })

	return nil
}

func (s *Store) selectMemories(
	query storage.MemoryQuery,
	matches func(domain.Memory) bool,
) []domain.Memory {
	at := query.At
	if at.IsZero() {
		at = time.Now()
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var found []domain.Memory
	for _, id := range s.memoryOrder {
		candidate, ok := s.memories[id]
		if !ok {
			continue
		}
		// Believed at that moment, on both timelines — the same rule the
		// database applies, expressed the same way, because two stores that
		// disagree about what is current is a bug nothing else can catch.
		if !query.IncludeInvalidated && !candidate.CurrentAt(at) {
			continue
		}
		if query.Activation != "" && candidate.Activation != query.Activation {
			continue
		}
		if len(query.Scopes) > 0 && !inScope(candidate, query.Scopes) {
			continue
		}
		if !matches(candidate) {
			continue
		}
		found = append(found, candidate)
	}

	// Newest first, matching what the database returns.
	sort.SliceStable(found, func(a, b int) bool {
		return found[a].CreatedAt.After(found[b].CreatedAt)
	})

	if query.Limit > 0 && len(found) > query.Limit {
		found = found[:query.Limit]
	}
	return found
}

func inScope(candidate domain.Memory, scopes []storage.MemoryScopeRef) bool {
	return slices.ContainsFunc(scopes, func(scope storage.MemoryScopeRef) bool {
		return candidate.Scope == scope.Scope && candidate.ScopeRef == scope.Ref
	})
}

// ExpireMemories stops believing what has gone unused past its expiry.
func (s *Store) ExpireMemories(_ context.Context, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	expired := 0
	for id, memory := range s.memories {
		if memory.InvalidatedAt != nil || memory.ExpiresAt == nil {
			continue
		}
		if at.Before(*memory.ExpiresAt) {
			continue
		}
		when := at
		memory.InvalidatedAt = &when
		s.memories[id] = memory
		expired++
	}
	return expired, nil
}

// TouchMemories records that these were used, and pushes their expiry out by
// the lifetime they were given.
func (s *Store) TouchMemories(_ context.Context, ids []domain.MemoryID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, id := range ids {
		memory, ok := s.memories[id]
		if !ok || memory.InvalidatedAt != nil {
			continue
		}

		if memory.ExpiresAt != nil {
			// The lifetime it was given, measured from whenever it was last
			// used. A memory written without one is not given one by being
			// read.
			since := memory.CreatedAt
			if memory.LastUsedAt != nil {
				since = *memory.LastUsedAt
			}
			extended := at.Add(memory.ExpiresAt.Sub(since))
			memory.ExpiresAt = &extended
		}

		used := at
		memory.LastUsedAt = &used
		s.memories[id] = memory
	}
	return nil
}
