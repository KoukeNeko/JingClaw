package gateway

import (
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// staleLogAge is how old a log line may be and still be worth posting.
const staleLogAge = 10 * time.Minute

// staleLog reports a log line too old to be context for anything.
//
// Only log lines: an answer, a question or an approval is owed however late
// it arrives, and a status line is rewritten in place rather than stacked. A
// log line is subtext for what is happening now, and one delivered an hour
// late after an outage is a wall of it under an answer somebody already read.
// Two hundred and twenty-nine of them arrived at once the first time the
// wire carried them at all.
func staleLog(dispatch gateway.Dispatch, now time.Time) bool {
	if dispatch.Kind != gateway.DispatchLog || dispatch.CreatedAt.IsZero() {
		return false
	}
	return now.Sub(dispatch.CreatedAt) > staleLogAge
}
