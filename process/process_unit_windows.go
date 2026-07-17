//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
	processJobs     sync.Map // map[*exec.Cmd]windows.Handle
)

func configureProcessUnit(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Starting suspended closes the descendant-spawn race between CreateProcess
	// and AssignProcessToJobObject. startProcessUnit resumes only after the
	// kill-on-close job owns the provider.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

func startProcessUnit(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	fail := func(cause error, job windows.Handle) error {
		if job != 0 {
			_ = windows.TerminateJobObject(job, 1)
			_ = windows.CloseHandle(job)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return cause
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fail(fmt.Errorf("create provider process job: %w", err), 0)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fail(fmt.Errorf("configure provider process job: %w", err), job)
	}

	access := uint32(windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE | windows.PROCESS_SUSPEND_RESUME)
	processHandle, err := windows.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(fmt.Errorf("open suspended provider process: %w", err), job)
	}
	defer windows.CloseHandle(processHandle)
	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		return fail(fmt.Errorf("assign provider process job: %w", err), job)
	}
	processJobs.Store(cmd, job)
	if err := ctx.Err(); err != nil {
		processJobs.Delete(cmd)
		return fail(fmt.Errorf("provider process cancelled before resume: %w", err), job)
	}
	status, _, _ := ntResumeProcess.Call(uintptr(processHandle))
	if status != 0 {
		processJobs.Delete(cmd)
		return fail(fmt.Errorf("resume contained provider process: %w", windows.NTStatus(status)), job)
	}
	return nil
}

func cancelProcessUnit(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if value, ok := processJobs.LoadAndDelete(cmd); ok {
		job := value.(windows.Handle)
		terminateErr := windows.TerminateJobObject(job, 1)
		closeErr := windows.CloseHandle(job)
		return errors.Join(terminateErr, closeErr)
	}
	return cmd.Process.Kill()
}
