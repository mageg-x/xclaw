//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func applyNativeSandbox(cmd *exec.Cmd, opts nativeSandboxOptions) {
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		attr.Cloneflags = syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS
		attr.Unshareflags = syscall.CLONE_NEWNS
		if opts.WorkspaceAccess != "none" && strings.TrimSpace(opts.WorkspaceDir) != "" {
			attr.Chroot = opts.WorkspaceDir
			attr.Credential = &syscall.Credential{Uid: 65534, Gid: 65534}
			cmd.Dir = "/"
		}
	}
	cmd.SysProcAttr = attr
}

func onNativeProcessStarted(pid int, opts nativeSandboxOptions) {
	_ = applyLinuxRlimits(pid)
	_ = applyLinuxCgroup(pid, opts)
}

func applyLinuxRlimits(pid int) error {
	mem := &unix.Rlimit{Cur: 2 << 30, Max: 2 << 30}
	cpu := &unix.Rlimit{Cur: 120, Max: 120}
	proc := &unix.Rlimit{Cur: 256, Max: 256}
	_ = unix.Prlimit(pid, unix.RLIMIT_AS, mem, nil)
	_ = unix.Prlimit(pid, unix.RLIMIT_CPU, cpu, nil)
	_ = unix.Prlimit(pid, unix.RLIMIT_NPROC, proc, nil)
	return nil
}

func applyLinuxCgroup(pid int, opts nativeSandboxOptions) error {
	root := "/sys/fs/cgroup"
	if _, err := os.Stat(root); err != nil {
		return nil
	}
	group := filepath.Join(root, "xclaw", strconv.Itoa(pid))
	if err := os.MkdirAll(group, 0o755); err != nil {
		return nil
	}
	_ = os.WriteFile(filepath.Join(group, "memory.max"), []byte("2147483648"), 0o644)
	_ = os.WriteFile(filepath.Join(group, "cpu.max"), []byte("200000 1000000"), 0o644)
	if err := os.WriteFile(filepath.Join(group, "cgroup.procs"), []byte(fmt.Sprintf("%d", pid)), 0o644); err != nil {
		return nil
	}
	return nil
}
