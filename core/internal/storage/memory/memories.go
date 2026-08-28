package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

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
	s.mu.RLock()
	defer s.mu.RUnlock()

	var found []domain.Memory
	for _, id := range s.memoryOrder {
		candidate, ok := s.memories[id]
		if !ok {
			continue
		}
		if !query.IncludeInvalidated && candidate.InvalidatedAt != nil {
			continue
		}
		if query.Kind != "" && candidate.Kind != query.Kind {
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
