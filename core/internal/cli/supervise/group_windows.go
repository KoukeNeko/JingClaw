//go:build windows

package supervise

import (
	"os/exec"
	"syscall"

	"github.com/KoukeNeko/JingClaw/core/internal/winjob"
)

// procGroup contains a part and everything it spawns in a Windows job object.
//
// Windows has no process group in the POSIX sense, and no Setpgid to ask for
// one. A job object is what holds a part's tree instead, so stopping the part
// takes the children it started with it rather than leaving them holding a
// port or a database.
type procGroup struct {
	job *winjob.Job
}

func newProcGroup() (*procGroup, error) {
	job, err := winjob.New()
	if err != nil {
		return nil, err
	}
	return &procGroup{job: job}, nil
}

// configure asks for the part to start suspended, so started can place it in
// the job before it can spawn a child that would escape it.
func (g *procGroup) configure(command *exec.Cmd) {
	winjob.Configure(command)
}

// started assigns the running part to the job and lets it go.
func (g *procGroup) started(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if err := g.job.Assign(command.Process.Pid); err != nil {
		return err
	}
	return winjob.Resume(command.Process.Pid)
}

// askToStop ends the tree. There is no graceful signal to send: Windows has no
// SIGTERM, and the console control events that come closest do not reach a
// process started without a console.
func (g *procGroup) askToStop(*exec.Cmd) { _ = g.job.Terminate() }

// kill ends the tree, the same as askToStop with nothing gentler to try first.
func (g *procGroup) kill(*exec.Cmd) { _ = g.job.Terminate() }

// close releases the job, ending anything still in it that a stop did not.
func (g *procGroup) close() { g.job.Close() }

// hangup is what a terminal going away would send, where terminals send that.
// Windows does not, and Go's signal package will not deliver it. Named here so
// the list of ways to leave reads the same on both platforms.
const hangup = syscall.SIGHUP
