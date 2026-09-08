//go:build windows

package process

import (
	"os/exec"

	"github.com/KoukeNeko/JingClaw/core/internal/winjob"
)

// procGroup contains the process and everything it spawns in a Windows job
// object, so that stopping the process stops the tree rather than orphaning
// whatever it started.
type procGroup struct {
	job *winjob.Job
}

// newProcGroup creates the job the process will be assigned to, before it is
// started so there is something to assign it to the moment it exists.
func newProcGroup() (*procGroup, error) {
	job, err := winjob.New()
	if err != nil {
		return nil, err
	}
	return &procGroup{job: job}, nil
}

// configure asks for the process to start suspended, so started can assign it
// before its first instruction runs — closing the window in which it could
// spawn a child that escapes the job.
func (g *procGroup) configure(command *exec.Cmd) {
	winjob.Configure(command)
}

// started assigns the running process to the job and lets it go.
func (g *procGroup) started(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if err := g.job.Assign(command.Process.Pid); err != nil {
		return err
	}
	return winjob.Resume(command.Process.Pid)
}

// containRunning puts an already-running process into the job — one this
// package did not start through os/exec, such as a program a pseudo console
// created. Unlike a command, it cannot be started suspended and assigned before
// its first instruction, so a child it spawns in the moment before assignment
// could escape; the window is small and the job takes everything after.
func (g *procGroup) containRunning(pid int) error {
	return g.job.Assign(pid)
}

// terminate ends the whole tree at once.
func (g *procGroup) terminate(*exec.Cmd) error {
	return g.job.Terminate()
}

// close releases the job, ending anything still in it that terminate did not.
func (g *procGroup) close() {
	g.job.Close()
}
