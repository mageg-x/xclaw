//go:build windows

package sandbox

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	jobMu      sync.Mutex
	jobHandles = make(map[int]windows.Handle)
)

func applyNativeSandbox(cmd *exec.Cmd, opts nativeSandboxOptions) {
	_ = opts
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

func onNativeProcessStarted(pid int, opts nativeSandboxOptions) {
	_ = opts
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	limits.ProcessMemoryLimit = uintptr(2 << 30)
	limits.BasicLimitInformation.ActiveProcessLimit = 1
	_, _ = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	)

	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(process)

	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return
	}

	jobMu.Lock()
	jobHandles[pid] = job
	jobMu.Unlock()
}
