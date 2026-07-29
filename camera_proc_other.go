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

// camDeprioritizeProcess drops a camera subprocess to a low CPU priority.
//
// The proxy serves interactive AI traffic by spawning UPSTREAM CLI CHILDREN
// (codex/claude/agy) in the same cgroup as the camera ffmpeg processes. A fleet
// of persistent stream-archiver ffmpegs will happily consume every core, and
// because the scheduler treats them as equal to those CLI children, live
// conversations start timing out while footage is being archived. Camera work is
// batch work and can always wait; a conversation cannot. Renicing the whole
// process group means ffmpeg only gets the CPU nobody else wants.
//
// Best-effort: a failure here is not worth failing a capture over, and an
// unprivileged process cannot lower its own niceness again anyway.
func camDeprioritizeProcess(cmd *exec.Cmd, nice int) {
	if cmd == nil || cmd.Process == nil || nice <= 0 {
		return
	}
	// PRIO_PROCESS, not PRIO_PGRP: only runCamCommand puts its child in a fresh
	// process group (configureCamProcAttr). The persistent stream workers do not,
	// so their pid is NOT a pgid and a PRIO_PGRP call would silently renice
	// nothing — which is exactly what happened the first time this shipped.
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, nice)
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
