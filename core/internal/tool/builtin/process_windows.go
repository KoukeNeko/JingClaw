//go:build windows

package builtin

import (
	"os/exec"

	"github.com/KoukeNeko/JingClaw/core/internal/winjob"
)

// processGroup contains a command and everything it spawns in a Windows job
// object, so that cancelling the command stops the tree rather than leaving
// descendants behind.
//
// Windows has no process group in the POSIX sense; a job object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE is the nearest equivalent that actually
// holds a process's children. The same building block contains the daemon's
// managed processes.
type processGroup struct {
	job *winjob.Job
}

// newProcessGroup creates the job the command will be assigned to, before it is
// started so there is something to assign it to the moment it exists.
func newProcessGroup() (*processGroup, error) {
	job, err := winjob.New()
	if err != nil {
		return nil, err
	}
	return &processGroup{job: job}, nil
}

// configure asks for the command to start suspended, so started can assign it
// before its first instruction runs — closing the window in which it could
// spawn a child that escapes the job.
func (g *processGroup) configure(command *exec.Cmd) {
	winjob.Configure(command)
}

// started assigns the running process to the job and lets it go.
func (g *processGroup) started(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if err := g.job.Assign(command.Process.Pid); err != nil {
		return err
	}
	return winjob.Resume(command.Process.Pid)
}

// terminate asks the command to stop, gently where it can.
//
// Windows has no SIGTERM. A console break is the nearest graceful stop: it
// reaches a command in its own process group and lets it flush before the
// caller's WaitDelay escalates to a kill. Where the break cannot be delivered —
// a daemon with no console — end the whole tree at once instead. The job still
// closes behind either path, so nothing outlives the command.
func (g *processGroup) terminate(command *exec.Cmd) error {
	if command.Process != nil {
		if err := winjob.SignalBreak(command.Process.Pid); err == nil {
			return nil
		}
	}
	return g.job.Terminate()
}

// close releases the job, ending anything still in it that terminate did not.
func (g *processGroup) close() {
	g.job.Close()
}
