//go:build windows

package main

// camera_proc_windows.go — Windows half of the ffmpeg/ffprobe process-tree kill
// used by runCamCommand (camera_capture.go). See camera_proc_other.go for the
// rationale and the Unix implementation; this file exists only because
// syscall.SysProcAttr's fields differ per-OS and can't be shared, mirroring the
// existing background_windows.go / background_other.go split in this codebase.

import (
	"os/exec"
	"strconv"
	"syscall"
)

// configureCamProcAttr hides the console window (captures run silently) and
// starts the process in its own process group so it doesn't receive console
// control events (e.g. Ctrl-C) intended for the proxy itself.
func configureCamProcAttr(cmd *exec.Cmd) {
	const createNewProcessGroup = 0x00000200
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup}
}

// killCamProcessTree force-kills cmd's process AND every process it spawned.
// This matters because some ffmpeg/ffprobe installs are a launcher/shim whose
// real worker process has a DIFFERENT pid than cmd.Process.Pid (observed with a
// Chocolatey-installed ffmpeg on the Windows dev box): a plain Process.Kill()
// only terminates the shim, leaving the actual ffmpeg process — and the
// stdout/stderr pipe handles it inherited — running indefinitely. That both
// leaks the process and hangs Cmd.Wait() forever waiting for pipe EOF that will
// never come (cmd.WaitDelay is set as a second line of defense in
// runCamCommand, but this tree-kill is what actually stops the leak).
// `taskkill /T` walks the whole process tree rooted at the pid.
func killCamProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
