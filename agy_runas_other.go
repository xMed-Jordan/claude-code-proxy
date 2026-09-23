//go:build !linux

package main

// agy_runas_other.go — the non-Linux stub half of PROXY_AGY_RUN_AS. Running
// agy as another user needs a real Linux credential syscall
// (syscall.Credential{Uid,Gid,Groups}); this proxy's dev box is Windows, and
// there is no equivalent notion of "start this child process as a different
// unprivileged OS user" to fall back to safely. agyResolveRunAs (agy.go)
// already refuses to mark run-as ready on any non-Linux GOOS, so this file's
// agyPrepareCmd only ever reaches its error branch in production — it exists
// so the fail-closed contract holds even if that startup check is ever
// weakened, and so this whole feature compiles and its cross-platform pieces
// (agyRunAsEnv, agyValidateExecutableBy, agyGrantDirs, ...) can be unit
// tested here on Windows.

import (
	"fmt"
	"os/exec"
)

// agyPrepareCmd: see agy_runas_linux.go for the real (Linux) implementation.
// Disabled: no-op. Enabled: always errors — this OS cannot run agy as
// another user, and the caller must not start cmd.
func agyPrepareCmd(cmd *exec.Cmd) error {
	if !agyRunAsRequested {
		return nil
	}
	return fmt.Errorf("agy run-as user %q is not ready: %s", agyRunAsConfiguredName, agyRunAsFailReason)
}

// realAgyStat: see agy_runas_linux.go. Never actually called in production
// off Linux (agyResolveRunAs fails closed on GOOS check before reaching any
// stat call), but must exist so agy.go's agyStatFn default compiles here.
func realAgyStat(path string) (agyPathStat, error) {
	return agyPathStat{}, fmt.Errorf("stat %s: run-as ownership checks are only supported on Linux", path)
}
