//go:build !linux && !darwin && !windows

package sandbox

import "os/exec"

func applyNativeSandbox(cmd *exec.Cmd, opts nativeSandboxOptions) {
	_ = cmd
	_ = opts
}

func onNativeProcessStarted(pid int, opts nativeSandboxOptions) {
	_ = pid
	_ = opts
}
