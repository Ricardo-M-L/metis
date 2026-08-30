//go:build windows

package builtin

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"github.com/Ricardo-M-L/metis/internal/security"
	"golang.org/x/sys/windows"
)

type webBrowseProcessTreeHandle struct{ job windows.Handle }

func attachWebBrowseProcessTree(cmd *exec.Cmd) *webBrowseProcessTreeHandle {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}
	err = windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}
	return &webBrowseProcessTreeHandle{job: job}
}

func terminateWebBrowseProcessTreeHandle(tree *webBrowseProcessTreeHandle) {
	if tree == nil || tree.job == 0 {
		return
	}
	_ = windows.TerminateJobObject(tree.job, 1)
}

func closeWebBrowseProcessTreeHandle(tree *webBrowseProcessTreeHandle) {
	if tree == nil || tree.job == 0 {
		return
	}
	_ = windows.CloseHandle(tree.job)
	tree.job = 0
}

var webBrowseTaskkillCommandContext = exec.CommandContext

// taskkill /T is the fallback when Windows refuses nested Job Object
// assignment. Its own CommandContext and WaitDelay prevent cleanup from
// replacing a stuck browser wait with a stuck taskkill wait.
func killWebBrowseProcessTreePlatform(process *os.Process, limit time.Duration) {
	if process == nil || process.Pid <= 0 || limit <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	taskkill := webBrowseTaskkillCommandContext(ctx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(process.Pid))
	taskkill.Env = security.RestrictedSubprocessEnv(os.Environ())
	taskkill.WaitDelay = limit
	_ = taskkill.Run()
}
