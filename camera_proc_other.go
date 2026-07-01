//go:build !windows

package main

// camera_proc_other.go — Unix half of the ffmpeg/ffprobe process-tree kill used
// by runCamCommand (camera_capture.go). See camera_proc_windows.go for the
// Windows implementation and the rationale (a launcher/shim's real worker
// process can have a different pid than cmd.Process.Pid, so killing just that
// one pid can leak an orphaned ffmpeg process and hang Cmd.Wait()). On
// production Linux installs ffmpeg is normally a plain binary with no such
// shim, but starting it in its own process group costs nothing and also
// catches the case where ffmpeg itself forks a helper.

import (
	"os/exec"
	"syscall"
)

// configureCamProcAttr starts the process in its own process group (setpgid) so
// killCamProcessTree can signal the whole group, not just the immediate pid.
func configureCamProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killCamProcessTree sends SIGKILL to the process group rooted at cmd's pid
// (see configureCamProcAttr) so any child the launched process spawned is
// killed too, not just the immediate exec'd process.
func killCamProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
