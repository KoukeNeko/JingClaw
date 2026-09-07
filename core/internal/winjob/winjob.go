//go:build windows

// Package winjob contains a process tree in a Windows job object.
//
// Windows has no process group in the POSIX sense. A job object is the nearest
// equivalent that actually holds descendants: a process assigned to one draws
// its children in with it, and JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE means the
// tree cannot outlive the job's handle even if the program that owns it is
// itself killed. It is shared so the agent's managed processes and the daemon's
// own parts contain their trees the same way.
//
// The order of use closes a race: a job is created, the process is started
// suspended so it cannot spawn a child before being assigned, Assign puts it in
// the job, and Resume lets it run.
package winjob

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job is a created, configured job object.
type Job struct {
	handle windows.Handle
}

// New creates a job whose processes end when the job's handle is closed.
func New() (*Job, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("winjob: create: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("winjob: configure: %w", err)
	}

	return &Job{handle: handle}, nil
}

// Configure asks for the command to start suspended, so Assign can place it in
// a job before its first instruction runs, and in its own process group, so a
// console break can be aimed at the tree without also reaching this process.
func Configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// Assign puts a running process into the job.
func (j *Job) Assign(pid int) error {
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false, uint32(pid))
	if err != nil {
		return fmt.Errorf("winjob: open %d to contain it: %w", pid, err)
	}
	defer windows.CloseHandle(process)

	if err := windows.AssignProcessToJobObject(j.handle, process); err != nil {
		return fmt.Errorf("winjob: assign %d: %w", pid, err)
	}
	return nil
}

// SignalBreak asks a process group to stop the gentle way Windows allows.
//
// It is the nearest thing to a SIGTERM: a CTRL_BREAK reaches a process started
// in its own group (see Configure) and gives it a chance to shut down before it
// is killed. The break only lands on a process that shares a console with this
// one, so a daemon started without a console cannot deliver it and the caller
// has to fall back to Terminate. The process group id is the root process's pid.
func SignalBreak(pid int) error {
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)); err != nil {
		return fmt.Errorf("winjob: break %d: %w", pid, err)
	}
	return nil
}

// Terminate ends every process in the job at once.
func (j *Job) Terminate() error {
	if err := windows.TerminateJobObject(j.handle, 1); err != nil {
		return fmt.Errorf("winjob: terminate: %w", err)
	}
	return nil
}

// Close releases the job handle, which — because of KILL_ON_JOB_CLOSE — also
// ends anything still in the job that Terminate did not.
func (j *Job) Close() {
	if j.handle != 0 {
		_ = windows.CloseHandle(j.handle)
		j.handle = 0
	}
}

// Resume starts every thread of a process created suspended.
//
// os/exec closes the thread handle it got from CreateProcess before it
// returns, so there is nothing to resume directly; the threads are found by
// walking a system-wide snapshot instead. A freshly created process has a
// single thread, but the walk does not rely on that.
func Resume(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("winjob: snapshot threads to resume %d: %w", pid, err)
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	owner := uint32(pid)
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != owner {
			continue
		}
		if resumeErr := resumeThread(entry.ThreadID); resumeErr != nil {
			return resumeErr
		}
	}
	// The walk ends by reporting no more entries, which is not a failure.
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("winjob: walk threads to resume %d: %w", pid, err)
	}
	return nil
}

func resumeThread(threadID uint32) error {
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, threadID)
	if err != nil {
		return fmt.Errorf("winjob: open thread %d to resume it: %w", threadID, err)
	}
	defer windows.CloseHandle(thread)

	if _, err := windows.ResumeThread(thread); err != nil {
		return fmt.Errorf("winjob: resume thread %d: %w", threadID, err)
	}
	return nil
}
