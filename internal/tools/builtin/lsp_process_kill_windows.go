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

// lspProcessTreeHandle owns a Windows Job Object configured with
// KILL_ON_JOB_CLOSE. taskkill remains the fallback for systems that refuse
// nested Job assignment, while successful assignment gives deterministic
// descendant cleanup without spawning another process during shutdown.
type lspProcessTreeHandle struct{ job windows.Handle }

func attachLSPProcessTree(cmd *exec.Cmd) *lspProcessTreeHandle {
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
	return &lspProcessTreeHandle{job: job}
}

func terminateLSPProcessTreeHandle(tree *lspProcessTreeHandle) {
	if tree == nil || tree.job == 0 {
		return
	}
	_ = windows.TerminateJobObject(tree.job, 1)
}

func closeLSPProcessTreeHandle(tree *lspProcessTreeHandle) {
	if tree == nil || tree.job == 0 {
		return
	}
	_ = windows.CloseHandle(tree.job)
	tree.job = 0
}

// Windows has no Unix process groups. taskkill /T walks the descendant tree;
// CommandContext bounds taskkill itself so cancellation cannot leak an
// unbounded goroutine or helper process.
func killLSPProcessTreePlatform(process *os.Process, limit time.Duration) {
	if process == nil || process.Pid <= 0 || limit <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	taskkill := exec.CommandContext(ctx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(process.Pid))
	taskkill.Env = security.RestrictedSubprocessEnv(os.Environ())
	taskkill.WaitDelay = limit
	_ = taskkill.Run()
}
