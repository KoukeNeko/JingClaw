package supervise

import (
	"context"
	"log/slog"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/gateway"
)

// pace is how a gateway that keeps failing is started again: after first, then
// after twice as long each time up to longest, going back to first once a run
// has lasted healthy.
type pace struct{ first, longest, healthy time.Duration }

var chatPace = pace{first: time.Second, longest: time.Minute, healthy: time.Minute}

// keepChatUp runs the gateway until ctx ends.
//
// The gateway is a goroutine of this process, so how it fails matters to the
// daemon beside it. A panic is caught and turned into an error (gateway.Contain)
// and, like any other error, is logged and followed by another try after a wait
// that doubles up to a limit; ending the process over it would take the daemon
// and the dashboard down with a crash-looping bot. The one thing that is not
// tried again is a configuration failure — a missing token, a platform it does
// not serve, a daemon in repair — because it is the same on every try until
// somebody writes it down: chat stays off, and that is said.
func keepChatUp(ctx context.Context, run func(context.Context) error, tell func(string), p pace) {
	wait := p.first
	for {
		began := time.Now()
		err := gateway.Contain(func() error { return run(ctx) })
		if ctx.Err() != nil {
			return
		}
		if gateway.IsUnconfigured(err) {
			// A chat nobody bound a room to was never turned on; the restart
			// advice is for somebody who did.
			if gateway.IsIdle(err) {
				tell("chat is off: " + err.Error())
				return
			}
			tell("chat is off: " + err.Error() +
				" (put that right, then restart: jingclaw stop, then jingclaw)")
			return
		}

		if time.Since(began) >= p.healthy {
			wait = p.first
		}
		if wait == p.first {
			// Once per streak, and without the error: it can quote a request,
			// and the log has it.
			tell("chat stopped unexpectedly; trying again (details in the log)")
		}
		slog.Error("the gateway stopped; starting it again", "error", err, "after", wait.String())

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, p.longest)
	}
}
