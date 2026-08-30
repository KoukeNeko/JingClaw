// Package memory implements storage.Store in memory.
//
// It exists so unit tests do not pay for a database, and so the SQLite
// implementation has something to be checked against: internal/storage runs
// the same conformance suite over both, which is how the two are kept
// behaviourally identical.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

type Store struct {
	mu       sync.RWMutex
	sessions map[domain.SessionID]domain.Session
	plans    map[domain.SessionID][]domain.PlanItem
	runs     map[domain.RunID]domain.Run
	events   map[domain.SessionID][]domain.Event

	// pruned is the highest sequence discarded per session, so numbering
	// survives pruning: a sequence identifies an event for the life of a
	// session, and reusing one after a prune would hand two different events
	// the same name.
	pruned map[domain.SessionID]domain.Seq

	// logSeq is the position of the last event appended anywhere, and
	// logPruned the highest position discarded. The same pair the SQLite
	// store keeps, because a difference between the two would be a difference
	// in what a client is told, not merely in how it is stored.
	logSeq    domain.Seq
	logPruned domain.Seq

	approvals map[domain.ApprovalID]domain.Approval
	questions map[domain.QuestionID]domain.Question

	// memoryOrder preserves insertion order, so a listing is stable when two
	// memories share a timestamp — which they do constantly in tests.
	memories    map[domain.MemoryID]domain.Memory
	memoryOrder []domain.MemoryID
}

var _ storage.Store = (*Store)(nil)

func New() *Store {
	return &Store{
		sessions:  make(map[domain.SessionID]domain.Session),
		questions: make(map[domain.QuestionID]domain.Question),
		plans:     make(map[domain.SessionID][]domain.PlanItem),
		runs:      make(map[domain.RunID]domain.Run),
		events:    make(map[domain.SessionID][]domain.Event),
		pruned:    make(map[domain.SessionID]domain.Seq),
		approvals: make(map[domain.ApprovalID]domain.Approval),
		memories:  make(map[domain.MemoryID]domain.Memory),
	}
}

func (s *Store) CreateSession(ctx context.Context, session domain.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sessions[session.ID]; exists {
		return storage.ErrDuplicateSession
	}
	s.sessions[session.ID] = session
	return nil
}

func (s *Store) Session(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[id]
	if !ok {
		return domain.Session{}, storage.ErrSessionNotFound
	}
	return session, nil
}

func (s *Store) SetSessionModel(
	ctx context.Context,
	id domain.SessionID,
	model string,
	at time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[id]
	if !ok {
		return storage.ErrSessionNotFound
	}
	session.Model = model
	session.UpdatedAt = at
	s.sessions[id] = session
	return nil
}

func (s *Store) Plan(ctx context.Context, session domain.SessionID) ([]domain.PlanItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Copied out. A caller that mutated what it was handed would change the
	// stored plan without going through SetPlan, and the event announcing the
	// change would never be written.
	return append([]domain.PlanItem{}, s.plans[session]...), nil
}

func (s *Store) SetPlan(
	ctx context.Context,
	session domain.SessionID,
	items []domain.PlanItem,
	_ time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.plans == nil {
		s.plans = map[domain.SessionID][]domain.PlanItem{}
	}
	s.plans[session] = append([]domain.PlanItem{}, items...)
	return nil
}

func (s *Store) ListSessions(ctx context.Context) ([]domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]domain.Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}

	// Newest first, matching the SQLite ordering.
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
		}
		return sessions[i].ID > sessions[j].ID
	})
	return sessions, nil
}

func (s *Store) CreateRun(ctx context.Context, run domain.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.runs[run.ID]; exists {
		return storage.ErrDuplicateRun
	}
	s.runs[run.ID] = run
	return nil
}

func (s *Store) UpdateRun(ctx context.Context, run domain.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.runs[run.ID]
	if !ok {
		return storage.ErrRunNotFound
	}

	// Mirror the SQLite statement, which only writes these two columns.
	existing.Status = run.Status
	existing.FinishedAt = run.FinishedAt
	s.runs[run.ID] = existing
	return nil
}

func (s *Store) Run(ctx context.Context, id domain.RunID) (domain.Run, error) {
	if err := ctx.Err(); err != nil {
		return domain.Run{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	run, ok := s.runs[id]
	if !ok {
		return domain.Run{}, storage.ErrRunNotFound
	}
	return run, nil
}

func (s *Store) ListRuns(ctx context.Context, session domain.SessionID) ([]domain.Run, error) {
	return s.filterRuns(ctx, func(run domain.Run) bool { return run.SessionID == session })
}

func (s *Store) UnfinishedRuns(ctx context.Context) ([]domain.Run, error) {
	return s.filterRuns(ctx, func(run domain.Run) bool { return !run.Status.IsTerminal() })
}

func (s *Store) filterRuns(ctx context.Context, keep func(domain.Run) bool) ([]domain.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var runs []domain.Run
	for _, run := range s.runs {
		if keep(run) {
			runs = append(runs, run)
		}
	}

	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
	return runs, nil
}

func (s *Store) Append(ctx context.Context, event domain.Event) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[event.SessionID]; !ok {
		return 0, storage.ErrSessionNotFound
	}

	events := s.events[event.SessionID]
	event.Seq = s.pruned[event.SessionID] + domain.Seq(len(events)) + 1

	s.logSeq++
	event.GlobalSeq = s.logSeq

	s.events[event.SessionID] = append(events, event)

	return event.Seq, nil
}

func (s *Store) ListAfter(ctx context.Context, id domain.SessionID, after domain.Seq, limit int) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.sessions[id]; !ok {
		return nil, storage.ErrSessionNotFound
	}

	events := s.events[id]
	if int(after) >= len(events) {
		return nil, nil
	}

	// Sequences are dense and 1-based, so seq N is at index N-1.
	tail := events[after:]
	if limit > 0 && len(tail) > limit {
		tail = tail[:limit]
	}

	out := make([]domain.Event, len(tail))
	copy(out, tail)
	return out, nil
}

func (s *Store) Head(ctx context.Context, id domain.SessionID) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.sessions[id]; !ok {
		return 0, storage.ErrSessionNotFound
	}
	return s.pruned[id] + domain.Seq(len(s.events[id])), nil
}

// Oldest is the earliest event still kept.
func (s *Store) Oldest(ctx context.Context, id domain.SessionID) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	events := s.events[id]
	if len(events) == 0 {
		return 0, nil
	}
	return events[0].Seq, nil
}

// PruneEvents discards everything at or below through.
func (s *Store) PruneEvents(
	ctx context.Context, id domain.SessionID, through domain.Seq,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if through <= 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	events := s.events[id]
	kept := events[:0]
	var removed int64
	for _, event := range events {
		if event.Seq <= through {
			removed++
			// Only ever upwards: sessions are pruned in no particular order,
			// so a later prune can be of older events.
			if event.GlobalSeq > s.logPruned {
				s.logPruned = event.GlobalSeq
			}
			continue
		}
		kept = append(kept, event)
	}

	s.events[id] = kept
	if removed > 0 && through > s.pruned[id] {
		if s.pruned == nil {
			s.pruned = make(map[domain.SessionID]domain.Seq)
		}
		s.pruned[id] = through
	}
	return removed, nil
}

func (s *Store) CreateApproval(ctx context.Context, approval domain.Approval) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.approvals == nil {
		s.approvals = make(map[domain.ApprovalID]domain.Approval)
	}
	for _, existing := range s.approvals {
		// Mirrors the unique index: one approval per tool call, so the same
		// prompt cannot be raised or answered twice.
		if existing.RunID == approval.RunID && existing.ToolCallID == approval.ToolCallID {
			return storage.ErrApprovalDecided
		}
	}

	s.approvals[approval.ID] = approval
	return nil
}

func (s *Store) Approval(ctx context.Context, id domain.ApprovalID) (domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	approval, ok := s.approvals[id]
	if !ok {
		return domain.Approval{}, storage.ErrApprovalNotFound
	}
	return approval, nil
}

func (s *Store) DecideApproval(
	ctx context.Context,
	id domain.ApprovalID,
	status domain.ApprovalStatus,
	scope domain.RememberScope,
	decidedBy domain.RunOrigin,
	at time.Time,
) (domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	approval, ok := s.approvals[id]
	if !ok {
		return domain.Approval{}, storage.ErrApprovalNotFound
	}
	if !approval.IsPending() {
		return domain.Approval{}, storage.ErrApprovalDecided
	}

	approval.Status = status
	approval.Scope = scope
	approval.DecidedBy = decidedBy
	approval.DecidedAt = &at
	s.approvals[id] = approval

	return approval, nil
}

func (s *Store) PendingApprovals(ctx context.Context, session domain.SessionID) ([]domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var pending []domain.Approval
	for _, approval := range s.approvals {
		if approval.SessionID == session && approval.IsPending() {
			pending = append(pending, approval)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].CreatedAt.Before(pending[j].CreatedAt)
		}
		return pending[i].ID < pending[j].ID
	})
	return pending, nil
}

func (s *Store) ApprovalForCall(ctx context.Context, run domain.RunID, call domain.ToolCallID) (domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, approval := range s.approvals {
		if approval.RunID == run && approval.ToolCallID == call {
			return approval, nil
		}
	}
	return domain.Approval{}, storage.ErrApprovalNotFound
}

func (s *Store) CreateQuestion(ctx context.Context, question domain.Question) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.questions == nil {
		s.questions = make(map[domain.QuestionID]domain.Question)
	}
	// One question per tool call, the same rule the schema enforces: two
	// prompts for one call would each resume the run.
	for _, existing := range s.questions {
		if existing.RunID == question.RunID && existing.ToolCallID == question.ToolCallID {
			return storage.ErrQuestionAnswered
		}
	}
	s.questions[question.ID] = question
	return nil
}

func (s *Store) Question(ctx context.Context, id domain.QuestionID) (domain.Question, error) {
	if err := ctx.Err(); err != nil {
		return domain.Question{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	question, ok := s.questions[id]
	if !ok {
		return domain.Question{}, storage.ErrQuestionNotFound
	}
	return question, nil
}

func (s *Store) QuestionForCall(
	ctx context.Context,
	run domain.RunID,
	call domain.ToolCallID,
) (domain.Question, error) {
	if err := ctx.Err(); err != nil {
		return domain.Question{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, question := range s.questions {
		if question.RunID == run && question.ToolCallID == call {
			return question, nil
		}
	}
	return domain.Question{}, storage.ErrQuestionNotFound
}

func (s *Store) PendingQuestions(
	ctx context.Context,
	session domain.SessionID,
) ([]domain.Question, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var pending []domain.Question
	for _, question := range s.questions {
		if question.SessionID == session && question.IsPending() {
			pending = append(pending, question)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})
	return pending, nil
}

func (s *Store) AnswerQuestion(
	ctx context.Context,
	id domain.QuestionID,
	status domain.QuestionStatus,
	answer string,
	answeredBy domain.RunOrigin,
	at time.Time,
) (domain.Question, error) {
	if err := ctx.Err(); err != nil {
		return domain.Question{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	question, ok := s.questions[id]
	if !ok {
		return domain.Question{}, storage.ErrQuestionNotFound
	}
	if !question.IsPending() {
		return domain.Question{}, storage.ErrQuestionAnswered
	}

	question.Status = status
	question.Answer = answer
	question.AnsweredBy = answeredBy
	question.AnsweredAt = &at
	s.questions[id] = question

	return question, nil
}

// ListAllAfter returns the whole log from a position in it.
func (s *Store) ListAllAfter(ctx context.Context, after domain.Seq, limit int) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []domain.Event
	for _, events := range s.events {
		for _, event := range events {
			if event.GlobalSeq > after {
				out = append(out, event)
			}
		}
	}

	// Gathered by session and then put back in the order they were written,
	// which is what the position is for.
	sort.Slice(out, func(i, j int) bool { return out[i].GlobalSeq < out[j].GlobalSeq })

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LogHead is the position of the last event appended anywhere.
func (s *Store) LogHead(ctx context.Context) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logSeq, nil
}

// LogPrunedThrough is the highest position that has been discarded.
func (s *Store) LogPrunedThrough(ctx context.Context) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logPruned, nil
}
