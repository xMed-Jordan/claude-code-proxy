//go:build linux

package main

// agy_runas_resolve_linux_test.go — v0.32.1, test-only: closes a gap found
// in review of v0.32.0. agy_runas_test.go's TestAgyValidateExecutableByPathTable
// exercises agyValidateExecutableBy directly, and
// TestAgyResolveRunAsFailsClosedOnLookupFailure exercises agyResolveRunAs's
// LOOKUP failure path — but nothing called agyResolveRunAs end to end with a
// SUCCESSFUL lookup and then a failing CLI/agyj validation, so a mutation
// that skipped the validation calls entirely (`agyValidateExecutableBy(...)`
// replaced with an always-nil error) left the whole suite green on Linux.
//
// This file must run on Linux — agyResolveRunAs returns early on any other
// GOOS, before ever reaching the CLI/agyj validation calls this test is
// about — so on Windows (this repo's dev box) it can only be type-checked
// via `GOOS=linux go vet ./...`; running it for real happens on the server.

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeStatPassExcept returns an agyStatFn-compatible func: a permissive
// (0755, root-owned but world-readable/traversable — like a normal /, /tmp,
// /home) stat for any path, EXCEPT the exact paths in blocked, which get a
// root-owned 0700 stat (no "other" access at all) — simulating a
// /root-style restrictive ancestor directory blocking traversal for anyone
// but root.
func fakeStatPassExcept(blocked ...string) func(string) (agyPathStat, error) {
	blockedSet := make(map[string]bool, len(blocked))
	for _, b := range blocked {
		blockedSet[b] = true
	}
	return func(p string) (agyPathStat, error) {
		if blockedSet[p] {
			return agyPathStat{Mode: fs.ModeDir | 0o700, UID: 0, GID: 0}, nil
		}
		mode := fs.FileMode(0o755)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			mode |= fs.ModeDir
		}
		return agyPathStat{Mode: mode, UID: 0, GID: 0}, nil
	}
}

// fakeRunAsLookup returns an agyLookupUserFn-compatible func resolving to a
// fixed non-root identity, regardless of the name asked for.
func fakeRunAsLookup(uid, gid int, home string) func(string) (int, int, []int, string, error) {
	return func(string) (int, int, []int, string, error) {
		return uid, gid, []int{gid}, home, nil
	}
}

// (a) cfg.AgyCLI resolves under a root-owned 0700 dir: agyRunAsReady must
// stay false, the fail reason must name the CLI path, and agyPrepareCmd
// must then refuse to prepare any command.
func TestAgyResolveRunAsEndToEndBadCLIFailsClosed(t *testing.T) {
	restoreState := snapshotRunAsState()
	t.Cleanup(restoreState)
	origLookup := agyLookupUserFn
	t.Cleanup(func() { agyLookupUserFn = origLookup })
	origStat := agyStatFn
	t.Cleanup(func() { agyStatFn = origStat })

	const uid, gid = 1500, 1500
	agyLookupUserFn = fakeRunAsLookup(uid, gid, "/home/agy")

	tmp := t.TempDir()
	restrictedDir := filepath.Join(tmp, "root-owned-bin")
	badCLI := filepath.Join(restrictedDir, "agy")
	if err := os.MkdirAll(restrictedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badCLI, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	agyStatFn = fakeStatPassExcept(restrictedDir)

	cfg := config{
		AgyRunAs:    "agy",
		AgyCLI:      badCLI,
		AgyBin:      "/does/not/matter/agyj", // never reached: the CLI check fails first
		AgyMediaDir: t.TempDir(),
		AgyImageDir: t.TempDir(),
	}
	agyResolveRunAs(cfg)

	if agyRunAsReady {
		t.Fatal("expected agyRunAsReady = false when the agy CLI is under a root-owned 0700 dir")
	}
	if !strings.Contains(agyRunAsFailReason, "agy CLI") || !strings.Contains(agyRunAsFailReason, badCLI) {
		t.Fatalf("fail reason = %q, want it to name the agy CLI and its path %q", agyRunAsFailReason, badCLI)
	}
	cmd := exec.Command("true")
	if err := agyPrepareCmd(cmd); err == nil {
		t.Fatal("expected agyPrepareCmd to refuse to prepare a command after a failed CLI validation")
	}
}

// (b) the agy CLI passes, but agyBinPath (the agyj wrapper) resolves under a
// root-owned 0700 dir: same fail-closed contract, with the reason naming
// the agyj wrapper this time.
func TestAgyResolveRunAsEndToEndBadAgyjFailsClosed(t *testing.T) {
	restoreState := snapshotRunAsState()
	t.Cleanup(restoreState)
	origLookup := agyLookupUserFn
	t.Cleanup(func() { agyLookupUserFn = origLookup })
	origStat := agyStatFn
	t.Cleanup(func() { agyStatFn = origStat })

	const uid, gid = 1500, 1500
	agyLookupUserFn = fakeRunAsLookup(uid, gid, "/home/agy")

	tmp := t.TempDir()
	goodDir := filepath.Join(tmp, "good-bin")
	goodCLI := filepath.Join(goodDir, "agy")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goodCLI, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	restrictedDir := filepath.Join(tmp, "root-owned-bin")
	badAgyj := filepath.Join(restrictedDir, "agyj") // agyBinPath never checks existence, so this need not exist on disk
	agyStatFn = fakeStatPassExcept(restrictedDir)

	cfg := config{
		AgyRunAs:    "agy",
		AgyCLI:      goodCLI,
		AgyBin:      badAgyj,
		AgyMediaDir: t.TempDir(),
		AgyImageDir: t.TempDir(),
	}
	agyResolveRunAs(cfg)

	if agyRunAsReady {
		t.Fatal("expected agyRunAsReady = false when the agyj wrapper is under a root-owned 0700 dir")
	}
	if !strings.Contains(agyRunAsFailReason, "agyj") || !strings.Contains(agyRunAsFailReason, badAgyj) {
		t.Fatalf("fail reason = %q, want it to name the agyj wrapper and its path %q", agyRunAsFailReason, badAgyj)
	}
	cmd := exec.Command("true")
	if err := agyPrepareCmd(cmd); err == nil {
		t.Fatal("expected agyPrepareCmd to refuse to prepare a command after a failed agyj validation")
	}
}

// (c) both the agy CLI and the agyj wrapper pass validation: agyRunAsReady
// must become true.
func TestAgyResolveRunAsEndToEndBothGoodReady(t *testing.T) {
	restoreState := snapshotRunAsState()
	t.Cleanup(restoreState)
	origLookup := agyLookupUserFn
	t.Cleanup(func() { agyLookupUserFn = origLookup })
	origStat := agyStatFn
	t.Cleanup(func() { agyStatFn = origStat })

	const uid, gid = 1500, 1500
	agyLookupUserFn = fakeRunAsLookup(uid, gid, "/home/agy")

	tmp := t.TempDir()
	goodDir := filepath.Join(tmp, "good-bin")
	goodCLI := filepath.Join(goodDir, "agy")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goodCLI, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	goodAgyj := filepath.Join(goodDir, "agyj") // agyBinPath never checks existence

	agyStatFn = fakeStatPassExcept() // nothing blocked — everything passes

	cfg := config{
		AgyRunAs:    "agy",
		AgyCLI:      goodCLI,
		AgyBin:      goodAgyj,
		AgyMediaDir: t.TempDir(),
		AgyImageDir: t.TempDir(),
	}
	agyResolveRunAs(cfg)

	if !agyRunAsReady {
		t.Fatalf("expected agyRunAsReady = true when both paths pass validation; fail reason = %q", agyRunAsFailReason)
	}
	if agyRunAsIdentityValue == nil {
		t.Fatal("agyRunAsIdentityValue is nil despite agyRunAsReady = true")
	}
	if agyRunAsIdentityValue.UID != uid || agyRunAsIdentityValue.GID != gid {
		t.Fatalf("identity = %+v, want uid=%d gid=%d", agyRunAsIdentityValue, uid, gid)
	}
}
