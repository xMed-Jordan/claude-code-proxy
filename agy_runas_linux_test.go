//go:build linux

package main

// agy_runas_linux_test.go — the one PROXY_AGY_RUN_AS test (v0.32.0) that
// genuinely needs a real Linux syscall.SysProcAttr: proving agyPrepareCmd
// sets Credential while PRESERVING a pre-existing SysProcAttr field
// (Setpgid) rather than clobbering it. Everything else about run-as is
// tested cross-platform in agy_runas_test.go — this file only runs when
// GOOS=linux (on the server, or via `GOOS=linux go vet ./...` /
// `GOOS=linux go build` here on the dev box, which at least type-checks it).

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestAgyPrepareCmdEnabledSetsCredentialKeepsExistingSetpgid(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()

	agyRunAsRequested = true
	agyRunAsReady = true
	agyRunAsConfiguredName = "agy"
	agyRunAsIdentityValue = &agyRunAsIdentity{
		Name: "agy", UID: 1500, GID: 1500, Groups: []int{1500, 27}, Home: "/home/agy",
	}

	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // simulates a caller that already needs process-group control

	if err := agyPrepareCmd(cmd); err != nil {
		t.Fatalf("agyPrepareCmd: %v", err)
	}

	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("agyPrepareCmd clobbered a pre-existing Setpgid instead of merging into it")
	}
	if cmd.SysProcAttr.Credential == nil {
		t.Fatal("Credential not set")
	}
	if cmd.SysProcAttr.Credential.Uid != 1500 {
		t.Fatalf("Credential.Uid = %d, want 1500", cmd.SysProcAttr.Credential.Uid)
	}
	if cmd.SysProcAttr.Credential.Gid != 1500 {
		t.Fatalf("Credential.Gid = %d, want 1500", cmd.SysProcAttr.Credential.Gid)
	}
	if len(cmd.SysProcAttr.Credential.Groups) != 2 || cmd.SysProcAttr.Credential.Groups[0] != 1500 || cmd.SysProcAttr.Credential.Groups[1] != 27 {
		t.Fatalf("Credential.Groups = %v, want [1500 27]", cmd.SysProcAttr.Credential.Groups)
	}

	if cmd.Dir != "/home/agy" {
		t.Fatalf("Dir = %q, want /home/agy (was empty, so agyPrepareCmd should have defaulted it)", cmd.Dir)
	}
	foundHome, foundUser, foundLogname := false, false, false
	for _, e := range cmd.Env {
		switch e {
		case "HOME=/home/agy":
			foundHome = true
		case "USER=agy":
			foundUser = true
		case "LOGNAME=agy":
			foundLogname = true
		}
	}
	if !foundHome || !foundUser || !foundLogname {
		t.Fatalf("env missing HOME/USER/LOGNAME: %v", cmd.Env)
	}
}

// TestAgyPrepareCmdPreservesExplicitDir confirms a caller-set cmd.Dir (e.g.
// spawnAgyWorker/runAgyStreamJSON's os.TempDir()) is left alone rather than
// overwritten with the run-as home.
func TestAgyPrepareCmdPreservesExplicitDir(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()

	agyRunAsRequested = true
	agyRunAsReady = true
	agyRunAsIdentityValue = &agyRunAsIdentity{Name: "agy", UID: 1500, GID: 1500, Home: "/home/agy"}

	cmd := exec.Command("true")
	cmd.Dir = "/tmp"

	if err := agyPrepareCmd(cmd); err != nil {
		t.Fatalf("agyPrepareCmd: %v", err)
	}
	if cmd.Dir != "/tmp" {
		t.Fatalf("Dir = %q, want /tmp (an explicit Dir must not be overwritten)", cmd.Dir)
	}
}
