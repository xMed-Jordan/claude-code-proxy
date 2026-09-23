package main

// agy_runas_test.go — tests for PROXY_AGY_RUN_AS (v0.32.0) that run on any
// platform (including this Windows dev box): config parsing, the pure
// agyRunAsEnv, the disabled-is-a-no-op contract, fail-closed behaviour (via
// the agyLookupUserFn seam — never touches a real OS user database),
// agyValidateExecutableBy's path-walk logic (via the agyStatFn seam),
// agyGrantDirs/agyGrantDirWritable (via the agyChownFn seam), agent
// definitions landing in the run-as home, and resolveAgyCLIPath's new
// candidate. See agy_runas_linux_test.go (linux build tag) for the one
// thing that genuinely needs a real Linux syscall.Credential.

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotRunAsState saves every package-level run-as var and returns a
// closure that restores them. Every test touching this global state must
// defer it immediately — Go tests in this file run sequentially, but the
// state is shared across all of them.
func snapshotRunAsState() func() {
	requested := agyRunAsRequested
	configuredName := agyRunAsConfiguredName
	ready := agyRunAsReady
	failReason := agyRunAsFailReason
	identity := agyRunAsIdentityValue
	homeHint := agyRunAsHomeHint
	return func() {
		agyRunAsRequested = requested
		agyRunAsConfiguredName = configuredName
		agyRunAsReady = ready
		agyRunAsFailReason = failReason
		agyRunAsIdentityValue = identity
		agyRunAsHomeHint = homeHint
	}
}

func TestParseAgyRunAs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		set  bool
		want string
	}{
		{"unset", "", false, ""},
		{"set-empty", "", true, ""},
		{"set-whitespace", "   ", true, ""},
		{"off", "off", true, ""},
		{"OFF-case-insensitive", "OFF", true, ""},
		{"none", "none", true, ""},
		{"root", "root", true, ""},
		{"ROOT-case-insensitive", "ROOT", true, ""},
		{"agy-user", "agy", true, "agy"},
		{"padded", "  agy  ", true, "agy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseAgyRunAs(c.raw, c.set); got != c.want {
				t.Fatalf("parseAgyRunAs(%q, %v) = %q, want %q", c.raw, c.set, got, c.want)
			}
		})
	}
}

func TestAgyRunAsEnvReplacesExistingKeys(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root", "SOMETHING=else"}
	got := agyRunAsEnv(base, "agy", "/home/agy")

	homeCount := 0
	for _, e := range got {
		if strings.HasPrefix(e, "HOME=") {
			homeCount++
			if e != "HOME=/home/agy" {
				t.Fatalf("HOME entry = %q, want HOME=/home/agy", e)
			}
		}
	}
	if homeCount != 1 {
		t.Fatalf("HOME appears %d times, want 1: %v", homeCount, got)
	}
	if !contains(got, "PATH=/usr/bin") || !contains(got, "SOMETHING=else") {
		t.Fatalf("unrelated env vars were dropped: %v", got)
	}
	if !contains(got, "USER=agy") || !contains(got, "LOGNAME=agy") {
		t.Fatalf("USER/LOGNAME not inserted: %v", got)
	}
}

func TestAgyRunAsEnvInsertsWhenAbsent(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	got := agyRunAsEnv(base, "agy", "/home/agy")
	for _, want := range []string{"HOME=/home/agy", "USER=agy", "LOGNAME=agy", "PATH=/usr/bin"} {
		if !contains(got, want) {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("len(got) = %d, want 4: %v", len(got), got)
	}
}

func TestAgyRunAsEnvNoDuplicateOnDuplicateInput(t *testing.T) {
	// Two pre-existing HOME= entries in base — exactly one HOME= must
	// survive, at the real replacement value.
	base := []string{"HOME=/root", "HOME=/tmp/other", "PATH=/usr/bin"}
	got := agyRunAsEnv(base, "agy", "/home/agy")
	count := 0
	for _, e := range got {
		if strings.HasPrefix(e, "HOME=") {
			count++
			if e != "HOME=/home/agy" {
				t.Fatalf("HOME entry = %q, want HOME=/home/agy", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("HOME appears %d times, want 1: %v", count, got)
	}
}

func TestAgyRunAsEnvNeverMutatesBase(t *testing.T) {
	base := []string{"HOME=/root"}
	baseCopy := append([]string(nil), base...)
	_ = agyRunAsEnv(base, "agy", "/home/agy")
	for i := range base {
		if base[i] != baseCopy[i] {
			t.Fatalf("base was mutated: %v vs original %v", base, baseCopy)
		}
	}
}

func TestAgyPrepareCmdDisabledLeavesCmdUntouched(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()
	agyRunAsRequested = false
	agyRunAsReady = false

	cmd := exec.Command("echo")
	origDir := cmd.Dir
	origSysProcAttr := cmd.SysProcAttr

	if err := agyPrepareCmd(cmd); err != nil {
		t.Fatalf("agyPrepareCmd (disabled) returned an error: %v", err)
	}
	if cmd.Env != nil {
		t.Fatalf("Env was set: %v", cmd.Env)
	}
	if cmd.Dir != origDir {
		t.Fatalf("Dir changed: %q", cmd.Dir)
	}
	if cmd.SysProcAttr != origSysProcAttr {
		t.Fatalf("SysProcAttr changed: %#v", cmd.SysProcAttr)
	}
}

// TestAgyResolveRunAsFailsClosedOnLookupFailure exercises the
// agyLookupUserFn seam directly (never touches a real OS user database, so
// this runs the same on Windows and Linux) and confirms the failure
// propagates all the way to agyPrepareCmd refusing to prepare a command —
// i.e. no process would ever be started.
func TestAgyResolveRunAsFailsClosedOnLookupFailure(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()
	origLookup := agyLookupUserFn
	defer func() { agyLookupUserFn = origLookup }()
	agyLookupUserFn = func(name string) (int, int, []int, string, error) {
		return 0, 0, nil, "", fmt.Errorf("no such user: %s", name)
	}

	agyResolveRunAs(config{AgyRunAs: "agy"})

	if agyRunAsReady {
		t.Fatal("expected agyRunAsReady = false after a lookup failure")
	}
	if !strings.Contains(agyRunAsFailReason, "no such user") {
		t.Fatalf("fail reason = %q, want it to mention the lookup error", agyRunAsFailReason)
	}

	cmd := exec.Command("does-not-matter")
	if err := agyPrepareCmd(cmd); err == nil {
		t.Fatal("expected agyPrepareCmd to return an error — no process may be started as root")
	} else if !strings.Contains(err.Error(), "agy") {
		t.Fatalf("error should name the run-as user: %v", err)
	}
}

func TestAgyResolveRunAsDisabledIsANoOp(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()
	agyResolveRunAs(config{AgyRunAs: ""})
	if agyRunAsRequested || agyRunAsReady {
		t.Fatalf("expected disabled state, got requested=%v ready=%v", agyRunAsRequested, agyRunAsReady)
	}
}

// TestAgyValidateExecutableByPathTable covers the path-walk logic via the
// agyStatFn seam — no real filesystem/syscall involved, so this runs on any
// platform.
func TestAgyValidateExecutableByPathTable(t *testing.T) {
	fake := map[string]agyPathStat{
		"/root":                      {Mode: 0o700, UID: 0, GID: 0},
		"/root/.local":               {Mode: 0o700, UID: 0, GID: 0},
		"/root/.local/bin":           {Mode: 0o700, UID: 0, GID: 0},
		"/root/.local/bin/agy":       {Mode: 0o755, UID: 0, GID: 0},
		"/home":                      {Mode: 0o755, UID: 0, GID: 0},
		"/home/agy":                  {Mode: 0o755, UID: 1500, GID: 1500},
		"/home/agy/.local":           {Mode: 0o755, UID: 1500, GID: 1500},
		"/home/agy/.local/bin":       {Mode: 0o755, UID: 1500, GID: 1500},
		"/home/agy/.local/bin/agy":   {Mode: 0o755, UID: 1500, GID: 1500},
		"/opt":                       {Mode: 0o755, UID: 0, GID: 0},
		"/opt/connect-ai-proxy":      {Mode: 0o755, UID: 0, GID: 0},
		"/opt/connect-ai-proxy/agyj": {Mode: 0o750, UID: 0, GID: 2000}, // group-only executable
	}
	orig := agyStatFn
	defer func() { agyStatFn = orig }()
	agyStatFn = func(path string) (agyPathStat, error) {
		st, ok := fake[path]
		if !ok {
			return agyPathStat{}, fmt.Errorf("no such path: %s", path)
		}
		return st, nil
	}

	cases := []struct {
		name     string
		path     string
		uid, gid int
		groups   []int
		wantErr  bool
	}{
		{"root-owned-path-fails", "/root/.local/bin/agy", 1500, 1500, []int{1500}, true},
		{"home-owned-by-user-passes", "/home/agy/.local/bin/agy", 1500, 1500, []int{1500}, false},
		{"group-only-passes-when-in-group", "/opt/connect-ai-proxy/agyj", 1500, 1500, []int{1500, 2000}, false},
		{"group-only-fails-when-not-in-group", "/opt/connect-ai-proxy/agyj", 1500, 1500, []int{1500}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := agyValidateExecutableBy(c.path, c.uid, c.gid, c.groups)
			if (err != nil) != c.wantErr {
				t.Fatalf("agyValidateExecutableBy(%s) err=%v, wantErr=%v", c.path, err, c.wantErr)
			}
		})
	}
}

func TestAgyGrantDirsChownsRestrictedTreeAndSkipsWorldReadable(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()
	agyRunAsReady = true
	agyRunAsIdentityValue = &agyRunAsIdentity{Name: "agy", UID: 1500, GID: 1500, Home: "/home/agy"}

	root := t.TempDir()
	restricted := filepath.Join(root, "f-abc123")
	pages := filepath.Join(restricted, "pages")
	if err := os.MkdirAll(pages, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restricted, "receipt.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pages, "page-001.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(restricted, 0o700); err != nil {
		t.Fatal(err)
	}

	worldReadable := filepath.Join(root, "f-world")
	if err := os.MkdirAll(worldReadable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldReadable, "x.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the restricted tree pointing OUTSIDE it — WalkDir
	// must chown the symlink's own path but never follow it.
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "outside.txt")
	symlinkPath := filepath.Join(restricted, "link")
	haveSymlink := true
	if err := os.WriteFile(outsideTarget, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, symlinkPath); err != nil {
		haveSymlink = false
		t.Logf("symlinks not supported in this environment, skipping that part of the check: %v", err)
	}

	// Fake agyStatFn for the "already accessible" DECISION specifically —
	// Windows' permission model does not map onto Unix mode bits the way
	// os.Chmod(dir, 0o700) above would imply on Linux, so the decision must
	// go through the same seam agyValidateExecutableBy uses rather than a
	// real os.Lstat here. Everything else (the actual chown walk below)
	// still exercises the real filesystem via filepath.WalkDir, with only
	// the terminal chown syscall faked via agyChownFn.
	origStat := agyStatFn
	defer func() { agyStatFn = origStat }()
	agyStatFn = func(p string) (agyPathStat, error) {
		switch p {
		case restricted:
			return agyPathStat{Mode: fs.ModeDir | 0o700, UID: 0, GID: 0}, nil
		case worldReadable:
			return agyPathStat{Mode: fs.ModeDir | 0o755, UID: 0, GID: 0}, nil
		}
		return agyPathStat{}, fmt.Errorf("unexpected stat of %s", p)
	}

	var calls []string
	origChown := agyChownFn
	defer func() { agyChownFn = origChown }()
	agyChownFn = func(name string, uid, gid int) error {
		if uid != 1500 || gid != 1500 {
			t.Fatalf("chown %s uid/gid = %d/%d, want 1500/1500", name, uid, gid)
		}
		calls = append(calls, name)
		return nil
	}

	if err := agyGrantDirs([]string{restricted, worldReadable}); err != nil {
		t.Fatalf("agyGrantDirs: %v", err)
	}

	for _, want := range []string{restricted, pages, filepath.Join(restricted, "receipt.png"), filepath.Join(pages, "page-001.png")} {
		if !contains(calls, want) {
			t.Fatalf("expected a chown call for %s, calls = %v", want, calls)
		}
	}
	for _, c := range calls {
		if strings.HasPrefix(c, worldReadable) {
			t.Fatalf("the world-readable tree must not be chowned, but got a call for %s", c)
		}
	}
	if haveSymlink {
		if !contains(calls, symlinkPath) {
			t.Fatalf("expected the symlink itself to be chowned: calls = %v", calls)
		}
		if contains(calls, outsideTarget) {
			t.Fatal("the symlink must never be followed — its external target must not be chowned")
		}
	}
}

func TestAgyGrantDirsNoOpWhenNotReady(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()
	agyRunAsReady = false
	agyRunAsIdentityValue = nil

	var called bool
	orig := agyChownFn
	defer func() { agyChownFn = orig }()
	agyChownFn = func(string, int, int) error { called = true; return nil }

	dir := t.TempDir()
	if err := agyGrantDirs([]string{dir}); err != nil {
		t.Fatalf("agyGrantDirs (not ready): %v", err)
	}
	if called {
		t.Fatal("agyChownFn must not be called when run-as is not ready")
	}
}

func TestAgyGrantDirWritableAlwaysChownsEvenIfReadable(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()
	agyRunAsReady = true
	agyRunAsIdentityValue = &agyRunAsIdentity{Name: "agy", UID: 1500, GID: 1500, Home: "/home/agy"}

	// A dir that LOOKS already-readable (0755) must STILL be chowned, since
	// agyGrantDirWritable never guesses from the mode — it is only called
	// when the caller already knows it needs write access.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var calls []string
	orig := agyChownFn
	defer func() { agyChownFn = orig }()
	agyChownFn = func(name string, uid, gid int) error {
		calls = append(calls, name)
		return nil
	}

	if err := agyGrantDirWritable(dir); err != nil {
		t.Fatalf("agyGrantDirWritable: %v", err)
	}
	if !contains(calls, dir) {
		t.Fatalf("expected %s to be chowned even though it looked readable: calls = %v", dir, calls)
	}
}

func TestEnsureAgyAgentDefinitionUsesRunAsHomeAndChowns(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()

	home := t.TempDir()
	agyRunAsReady = true
	agyRunAsIdentityValue = &agyRunAsIdentity{Name: "agy", UID: 1500, GID: 1500, Home: home}

	var calls []string
	origChown := agyChownFn
	defer func() { agyChownFn = origChown }()
	agyChownFn = func(name string, uid, gid int) error {
		calls = append(calls, name)
		return nil
	}

	ensureAgyAgentDefinition(config{AgyAgent: agyChatAgentName, AgyMediaAgent: agyMediaViewAgentName})

	chatDir := filepath.Join(home, ".gemini", "config", "agents", agyChatAgentName)
	mediaDir := filepath.Join(home, ".gemini", "config", "agents", agyMediaViewAgentName)
	if _, err := os.Stat(filepath.Join(chatDir, "agent.md")); err != nil {
		t.Fatalf("chat agent definition not written under the run-as home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "agent.md")); err != nil {
		t.Fatalf("media-view agent definition not written under the run-as home: %v", err)
	}
	if !contains(calls, chatDir) {
		t.Fatalf("expected the chat agent dir to be chowned, calls = %v", calls)
	}
	if !contains(calls, mediaDir) {
		t.Fatalf("expected the media-view agent dir to be chowned, calls = %v", calls)
	}
}

func TestResolveAgyCLIPathTriesRunAsHomeFirst(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()

	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	agyPath := filepath.Join(localBin, "agy")
	if err := os.WriteFile(agyPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	agyRunAsHomeHint = home
	// This dev box may have a real "agy"/"agy.exe" earlier in PATH (it
	// does) — clear it so exec.LookPath falls through to the candidate
	// list this test actually exercises, same as it would on a server with
	// no agy on PATH at all.
	t.Setenv("PATH", "")

	got := resolveAgyCLIPath(config{})
	if got != agyPath {
		t.Fatalf("resolveAgyCLIPath = %q, want %q (the run-as home candidate)", got, agyPath)
	}
}

func TestResolveAgyCLIPathExplicitAgyCLIStillWins(t *testing.T) {
	restore := snapshotRunAsState()
	defer restore()

	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localBin, "agy"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	agyRunAsHomeHint = home

	explicit := filepath.Join(t.TempDir(), "explicit-agy")
	if err := os.WriteFile(explicit, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolveAgyCLIPath(config{AgyCLI: explicit})
	if got != explicit {
		t.Fatalf("resolveAgyCLIPath with explicit AgyCLI = %q, want %q", got, explicit)
	}
}

// TestAgyPrepareCmdCalledAtAllThreeExecSites is a grep-based guard, not a
// behavioural test. Behaviourally proving "agyPrepareCmd runs before
// Start/Run at each of the three real agy exec sites" would need a seam
// distinguishing "about to spawn" from "already prepared" at each call
// site individually, on top of the seams this file already has — more
// refactor than this change justifies (see the v0.32.0 report, M6). This
// instead asserts the literal call is present in the source of each of the
// three spawn functions, so removing it (M6's mutation) is still caught.
func TestAgyPrepareCmdCalledAtAllThreeExecSites(t *testing.T) {
	checks := []struct{ file, funcName string }{
		{"agy.go", "runAgyjAgent"},
		{"agy_worker_pool.go", "spawnAgyWorker"},
		{"agy_worker_pool.go", "runAgyStreamJSON"},
	}
	for _, c := range checks {
		t.Run(c.funcName, func(t *testing.T) {
			body := extractFuncBody(t, c.file, c.funcName)
			if !strings.Contains(body, "agyPrepareCmd(cmd)") {
				t.Fatalf("%s in %s does not call agyPrepareCmd(cmd) before spawning", c.funcName, c.file)
			}
		})
	}
}

// extractFuncBody reads file and returns the source text of the named
// top-level function, from "func <name>(" to the closing brace at column 0
// — good enough for a grep-based guard, not a general Go parser.
func extractFuncBody(t *testing.T, file, name string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	src := string(raw)
	marker := "func " + name + "("
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("function %s not found in %s", name, file)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end+len("\n}\n")]
}
