package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

const questionColumns = `id, session_id, run_id, tool_call_id, prompt, kind, options,
	status, answer, answered_by, created_at, answered_at`

type questionOptionRow struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

func (s *Store) CreateQuestion(ctx context.Context, question domain.Question) error {
	options, err := json.Marshal(optionRows(question.Options))
	if err != nil {
		return fmt.Errorf("sqlite: encode options: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO questions (`+questionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(question.ID), string(question.SessionID), string(question.RunID),
		string(question.ToolCallID), question.Prompt, string(question.Kind), string(options),
		string(question.Status), question.Answer, question.AnsweredBy,
		question.CreatedAt.UnixNano(), nullableTime(question.AnsweredAt),
	)
	if isUniqueViolation(err) {
		return storage.ErrQuestionAnswered
	}
	if err != nil {
		return fmt.Errorf("sqlite: create question: %w", err)
	}
	return nil
}

func (s *Store) Question(ctx context.Context, id domain.QuestionID) (domain.Question, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+questionColumns+` FROM questions WHERE id = ?`, string(id))

	question, err := scanQuestion(row)
	if err != nil {
		return domain.Question{}, wrapNotFound(err, storage.ErrQuestionNotFound)
	}
	return question, nil
}

func (s *Store) QuestionForCall(
	ctx context.Context,
	run domain.RunID,
	call domain.ToolCallID,
) (domain.Question, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+questionColumns+` FROM questions WHERE run_id = ? AND tool_call_id = ?`,
		string(run), string(call))

	question, err := scanQuestion(row)
	if err != nil {
		return domain.Question{}, wrapNotFound(err, storage.ErrQuestionNotFound)
	}
	return question, nil
}

func (s *Store) PendingQuestions(
	ctx context.Context,
	session domain.SessionID,
) ([]domain.Question, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+questionColumns+` FROM questions
		 WHERE session_id = ? AND status = ? ORDER BY created_at, id`,
		string(session), string(domain.AnswerPending))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list questions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var questions []domain.Question
	for rows.Next() {
		question, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate questions: %w", err)
	}
	return questions, nil
}

// AnswerQuestion settles a pending question.
//
// The status check is in the UPDATE rather than in a preceding SELECT: two
// clients answering the same prompt at the same moment would both pass a
// read-then-write check, and the run would resume twice.
func (s *Store) AnswerQuestion(
	ctx context.Context,
	id domain.QuestionID,
	status domain.QuestionStatus,
	answer, answeredBy string,
	at time.Time,
) (domain.Question, error) {
	var question domain.Question

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE questions SET status = ?, answer = ?, answered_by = ?, answered_at = ?
			 WHERE id = ? AND status = ?`,
			string(status), answer, answeredBy, at.UnixNano(),
			string(id), string(domain.AnswerPending),
		)
		if err != nil {
			return fmt.Errorf("sqlite: answer question: %w", err)
		}

		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("sqlite: answer question: %w", err)
		}
		if affected == 0 {
			// Either it does not exist or somebody already answered. Told
			// apart by looking, so a mistyped id is not reported as a race.
			row := tx.QueryRowContext(ctx,
				`SELECT `+questionColumns+` FROM questions WHERE id = ?`, string(id))
			if _, scanErr := scanQuestion(row); scanErr != nil {
				return wrapNotFound(scanErr, storage.ErrQuestionNotFound)
			}
			return storage.ErrQuestionAnswered
		}

		row := tx.QueryRowContext(ctx,
			`SELECT `+questionColumns+` FROM questions WHERE id = ?`, string(id))
		question, err = scanQuestion(row)
		return err
	})
	if err != nil {
		return domain.Question{}, err
	}
	return question, nil
}

func optionRows(options []domain.QuestionOption) []questionOptionRow {
	rows := make([]questionOptionRow, 0, len(options))
	for _, option := range options {
		rows = append(rows, questionOptionRow{
			ID: option.ID, Label: option.Label, Detail: option.Detail,
		})
	}
	return rows
}

func scanQuestion(row scanner) (domain.Question, error) {
	var (
		question   domain.Question
		optionsRaw string
		created    int64
		answered   sql.NullInt64
	)

	if err := row.Scan(
		&question.ID, &question.SessionID, &question.RunID, &question.ToolCallID,
		&question.Prompt, &question.Kind, &optionsRaw,
		&question.Status, &question.Answer, &question.AnsweredBy,
		&created, &answered,
	); err != nil {
		return domain.Question{}, err
	}

	if optionsRaw != "" {
		var rows []questionOptionRow
		if err := json.Unmarshal([]byte(optionsRaw), &rows); err != nil {
			return domain.Question{}, fmt.Errorf("sqlite: decode options: %w", err)
		}
		for _, one := range rows {
			question.Options = append(question.Options, domain.QuestionOption{
				ID: one.ID, Label: one.Label, Detail: one.Detail,
			})
		}
	}

	question.CreatedAt = timeFromNanos(created)
	if answered.Valid {
		at := timeFromNanos(answered.Int64)
		question.AnsweredAt = &at
	}

	return question, nil
}
