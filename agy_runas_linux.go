//go:build linux

package main

// agy_runas_linux.go — the Linux-only half of PROXY_AGY_RUN_AS: applying the
// resolved run-as identity (agy.go's agyResolveRunAs / agyRunAsIdentityValue)
// to an *exec.Cmd before it starts, and reading real ownership/permission
// bits off the filesystem for agy.go's agyValidateExecutableBy. See
// agy_runas_other.go for the non-Linux stub (run-as always fails closed
// there — see agyResolveRunAs's runtime.GOOS check, which never lets this
// file's real logic matter off Linux in the first place).

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// agyPrepareCmd applies the resolved run-as identity to cmd before Start (or
// Run), at all three agy exec sites (runAgyjAgent's agyj-wrapper path,
// spawnAgyWorker, runAgyStreamJSON). Disabled (PROXY_AGY_RUN_AS unset):
// complete no-op, cmd is untouched. Enabled but not ready (resolution or
// startup validation failed — see agyResolveRunAs): returns an error naming
// the user and the cause; the caller must not start cmd — this is the
// fail-closed path, and it is the ONLY way run-as ever "fails": there is no
// path in this function that silently leaves cmd running as the caller's
// own (root) credentials once PROXY_AGY_RUN_AS is set.
//
// Enabled and ready: sets cmd.SysProcAttr.Credential, preserving any other
// SysProcAttr fields the caller already set (e.g. a future Setpgid); rewrites
// HOME/USER/LOGNAME in cmd.Env via the pure agyRunAsEnv (starting from
// os.Environ() if cmd.Env is nil); and defaults cmd.Dir to the user's home
// when the caller left it empty (a caller that already set cmd.Dir, e.g. to
// os.TempDir(), is left alone).
func agyPrepareCmd(cmd *exec.Cmd) error {
	if !agyRunAsRequested {
		return nil
	}
	if !agyRunAsReady || agyRunAsIdentityValue == nil {
		return fmt.Errorf("agy run-as user %q is not ready: %s", agyRunAsConfiguredName, agyRunAsFailReason)
	}
	id := agyRunAsIdentityValue

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	groups32 := make([]uint32, len(id.Groups))
	for i, g := range id.Groups {
		groups32[i] = uint32(g)
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid:    uint32(id.UID),
		Gid:    uint32(id.GID),
		Groups: groups32,
	}

	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	cmd.Env = agyRunAsEnv(base, id.Name, id.Home)

	if cmd.Dir == "" {
		cmd.Dir = id.Home
	}
	return nil
}

// realAgyStat is agy.go's agyStatFn default: the actual Linux stat, read via
// syscall.Stat_t for the real numeric uid/gid (os.FileInfo alone only
// exposes the permission bits portably, not ownership).
func realAgyStat(path string) (agyPathStat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return agyPathStat{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return agyPathStat{}, fmt.Errorf("%s: no Linux stat_t available", path)
	}
	// The FULL mode, not just .Perm(): callers (agyGrantDirs) also need the
	// type bit (fs.ModeDir) to know whether "other" read+execute means
	// "world-readable directory" or something else. .Perm() still extracts
	// just the permission bits correctly from a full mode when that's all a
	// caller (agyValidateExecutableBy) needs.
	return agyPathStat{Mode: info.Mode(), UID: int(st.Uid), GID: int(st.Gid)}, nil
}
