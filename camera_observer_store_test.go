package main

// camera_observer_store_test.go — the activity-log store (camera_observer_store.go):
// migration idempotency for the four log tables + the shared sync counter, CRUD
// round-trips, atomic claims (stream/hour/day — one winner), range/browse
// queries, and the pull-sync cursor contract: seq strictly increasing across
// mixed inserts into all three tables, never regressing across pruner deletes,
// terminal hour/day updates re-stamping seq (the row reappears past a
// pre-update cursor exactly once), pending rows hidden from sync, and
// cross-site isolation of the cursor queries. Temp sqlite only.

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

// openCamObserverTestDB opens a temp on-disk DB with the SAME pragmas as
// openProxyDB (busy timeout + WAL) so the concurrent-claim tests exercise the
// real locking behavior instead of failing instantly with SQLITE_BUSY.
func openCamObserverTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), ".proxy.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	return db
}

// ─────────────────────────── sync counter ───────────────────────────

// TestCamSyncCounterSurvivesRemigration proves the counter is created by the
// migration, hands out strictly increasing values, and is NOT reset by a
// re-migration (a proxy restart must never regress the sync cursor space).
func TestCamSyncCounterSurvivesRemigration(t *testing.T) {
	db := openCamObserverTestDB(t)

	if v, err := camSyncMaxSeq(db); err != nil || v != 0 {
		t.Fatalf("fresh counter = %d, %v; want 0, nil", v, err)
	}

	take := func() int64 {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		v, err := nextCamSyncSeq(tx)
		if err != nil {
			t.Fatalf("nextCamSyncSeq: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return v
	}
	if got := take(); got != 1 {
		t.Fatalf("first seq = %d, want 1", got)
	}
	if got := take(); got != 2 {
		t.Fatalf("second seq = %d, want 2", got)
	}

	// A re-migration (restart path) must leave the counter's value intact.
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if v, err := camSyncMaxSeq(db); err != nil || v != 2 {
		t.Fatalf("counter after re-migration = %d, %v; want 2", v, err)
	}
	if got := take(); got != 3 {
		t.Fatalf("seq after re-migration = %d, want 3", got)
	}

	// A rolled-back transaction returns its value to the counter.
	tx, _ := db.Begin()
	if _, err := nextCamSyncSeq(tx); err != nil {
		t.Fatalf("nextCamSyncSeq (rollback path): %v", err)
	}
	tx.Rollback()
	if v, _ := camSyncMaxSeq(db); v != 3 {
		t.Fatalf("counter after rollback = %d, want 3 (rollback must not consume)", v)
	}
}

// ─────────────────────────── stream CRUD + claims ───────────────────────────

func TestLogStreamCRUDAndClaim(t *testing.T) {
	db := openCamObserverTestDB(t)

	id, err := insertLogStream(db, logStream{
		SiteID: "site_a", Name: "Lobby journal", CameraIDs: `["cam_1","cam_2"]`,
		MotionGate: true, Enabled: true,
	})
	if err != nil {
		t.Fatalf("insertLogStream: %v", err)
	}
	s, err := getLogStream(db, id)
	if err != nil {
		t.Fatalf("getLogStream: %v", err)
	}
	// Zero-value knobs are defaulted at insert so a minimal row is runnable.
	if s.IntervalMinutes != 5 || s.FrameStepSeconds != 30 || s.DailyReportTime != "20:00" {
		t.Fatalf("defaults = interval %d step %d report %q, want 5/30/20:00", s.IntervalMinutes, s.FrameStepSeconds, s.DailyReportTime)
	}
	if !s.MotionGate || !s.Enabled || s.Name != "Lobby journal" {
		t.Fatalf("stream = %+v", s)
	}

	// Update the operator fields; scheduler columns stay untouched.
	s.Name = "Lobby"
	s.IntervalMinutes = 10
	s.FrameStepSeconds = 60
	s.MotionGate = false
	s.DailyReportTime = "21:30"
	if err := updateLogStream(db, s); err != nil {
		t.Fatalf("updateLogStream: %v", err)
	}
	got, _ := getLogStream(db, id)
	if got.Name != "Lobby" || got.IntervalMinutes != 10 || got.FrameStepSeconds != 60 || got.MotionGate || got.DailyReportTime != "21:30" {
		t.Fatalf("after update: %+v", got)
	}

	// Due list: empty next_run_at is due; a future one is not; disabled never is.
	due, err := listDueLogStreams(db, "2026-07-10T12:00:00Z")
	if err != nil || len(due) != 1 || due[0].ID != id {
		t.Fatalf("due = %+v, %v; want the one stream", due, err)
	}
	if err := setLogStreamEnabled(db, id, true, "2026-07-10T13:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if due, _ = listDueLogStreams(db, "2026-07-10T12:00:00Z"); len(due) != 0 {
		t.Fatalf("future next_run_at must not be due, got %+v", due)
	}
	if due, _ = listDueLogStreams(db, "2026-07-10T13:00:00Z"); len(due) != 1 {
		t.Fatalf("next_run_at == now must be due, got %+v", due)
	}
	if err := setLogStreamEnabled(db, id, false, ""); err != nil {
		t.Fatal(err)
	}
	if due, _ = listDueLogStreams(db, "2026-07-10T13:00:00Z"); len(due) != 0 {
		t.Fatalf("disabled stream must never be due, got %+v", due)
	}
	if err := setLogStreamEnabled(db, id, true, "2026-07-10T13:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// Atomic claim: exactly one caller advances next_run_at; a repeat of the
	// same expected value loses; a disabled stream cannot be claimed at all.
	ok, err := claimLogStream(db, id, "2026-07-10T13:00:00Z", "2026-07-10T13:10:00Z")
	if err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want win", ok, err)
	}
	ok, err = claimLogStream(db, id, "2026-07-10T13:00:00Z", "2026-07-10T13:20:00Z")
	if err != nil || ok {
		t.Fatalf("second claim = %v, %v; want loss (next_run_at already advanced)", ok, err)
	}
	if err := setLogStreamEnabled(db, id, false, "2026-07-10T13:10:00Z"); err != nil {
		t.Fatal(err)
	}
	if ok, _ = claimLogStream(db, id, "2026-07-10T13:10:00Z", "x"); ok {
		t.Fatal("a disabled stream must not be claimable")
	}
	if err := setLogStreamEnabled(db, id, true, "2026-07-10T13:10:00Z"); err != nil {
		t.Fatal(err)
	}

	// Cursor write: last_run/window/error move, next_run_at (the claim's
	// property) does not.
	before, _ := getLogStream(db, id)
	if err := setLogStreamCursor(db, id, "2026-07-10T13:10:05Z", "2026-07-10T13:09:00Z", "s3 misses: 2"); err != nil {
		t.Fatalf("setLogStreamCursor: %v", err)
	}
	after, _ := getLogStream(db, id)
	if after.LastRunAt != "2026-07-10T13:10:05Z" || after.LastWindowTo != "2026-07-10T13:09:00Z" || after.LastError != "s3 misses: 2" {
		t.Fatalf("cursor fields = %+v", after)
	}
	if after.NextRunAt != before.NextRunAt {
		t.Fatalf("setLogStreamCursor moved next_run_at %q → %q (must never touch the claim)", before.NextRunAt, after.NextRunAt)
	}

	// Site listing + delete.
	if _, err := insertLogStream(db, logStream{SiteID: "site_b", Name: "other site"}); err != nil {
		t.Fatal(err)
	}
	if ls, _ := listLogStreams(db, "site_a"); len(ls) != 1 || ls[0].ID != id {
		t.Fatalf("site_a streams = %+v", ls)
	}
	if ls, _ := listLogStreams(db, ""); len(ls) != 2 {
		t.Fatalf("all streams = %d, want 2", len(ls))
	}
	if err := deleteLogStream(db, id); err != nil {
		t.Fatalf("deleteLogStream: %v", err)
	}
	if _, err := getLogStream(db, id); err == nil {
		t.Fatal("stream must be gone after delete")
	}
}

// ─────────────────────────── seq contract ───────────────────────────

// TestLogSeqMonotonicAcrossTablesAndPrunes is the core sync-cursor invariant:
// one shared counter stamps strictly increasing seq values across MIXED writes
// into all three log tables, and pruner deletes never make it regress.
func TestLogSeqMonotonicAcrossTablesAndPrunes(t *testing.T) {
	db := openCamObserverTestDB(t)

	var seqs []int64
	record := func(read func() (int64, error)) {
		t.Helper()
		v, err := read()
		if err != nil {
			t.Fatalf("read seq: %v", err)
		}
		seqs = append(seqs, v)
	}

	e1, err := insertLogEntry(db, logEntry{StreamID: "ls_1", SiteID: "site_a", FromTS: "2026-07-10T10:00:00Z", ToTS: "2026-07-10T10:05:00Z", Headline: "quiet"})
	if err != nil {
		t.Fatalf("insertLogEntry: %v", err)
	}
	record(func() (int64, error) { e, err := getLogEntry(db, e1); return e.Seq, err })

	hourID, won, err := claimLogHour(db, logHour{StreamID: "ls_1", SiteID: "site_a", HourStart: "2026-07-10T10:00:00Z", TZ: "Asia/Amman"})
	if err != nil || !won {
		t.Fatalf("claimLogHour = %v, %v", won, err)
	}
	record(func() (int64, error) { h, err := getLogHour(db, hourID); return h.Seq, err })

	dayID, won, err := claimLogDay(db, logDay{StreamID: "ls_1", SiteID: "site_a", Day: "2026-07-10"})
	if err != nil || !won {
		t.Fatalf("claimLogDay = %v, %v", won, err)
	}
	record(func() (int64, error) { d, err := getLogDay(db, dayID); return d.Seq, err })

	e2, err := insertLogEntry(db, logEntry{StreamID: "ls_1", SiteID: "site_a", FromTS: "2026-07-10T10:05:00Z", ToTS: "2026-07-10T10:10:00Z", Headline: "still quiet"})
	if err != nil {
		t.Fatal(err)
	}
	record(func() (int64, error) { e, err := getLogEntry(db, e2); return e.Seq, err })

	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq not strictly increasing across tables: %v", seqs)
		}
	}
	maxBefore := seqs[len(seqs)-1]

	// Prune EVERYTHING (future cutoff) — the counter row must survive, so the
	// next stamped write continues past the old max instead of regressing.
	for name, del := range map[string]func(*sql.DB, string) (int64, error){
		"entries": deleteOldLogEntries, "hours": deleteOldLogHours, "days": deleteOldLogDays,
	} {
		if _, err := del(db, "2999-01-01T00:00:00Z"); err != nil {
			t.Fatalf("prune %s: %v", name, err)
		}
	}
	if entries, _ := listLogEntriesBySite(db, "site_a", "", "", "", "", 0); len(entries) != 0 {
		t.Fatalf("entries after prune = %d, want 0", len(entries))
	}
	e3, err := insertLogEntry(db, logEntry{StreamID: "ls_1", SiteID: "site_a", FromTS: "2026-07-10T10:10:00Z", ToTS: "2026-07-10T10:15:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := getLogEntry(db, e3)
	if e.Seq <= maxBefore {
		t.Fatalf("seq after full prune = %d, want > %d (counter must never regress)", e.Seq, maxBefore)
	}
}

// TestLogHourReStampAndPendingHidden covers the hour rollup's sync semantics:
// a pending claim is invisible to the cursor query; the terminal update
// re-stamps seq so the finished row surfaces past a pre-update cursor exactly
// once; and a second claim of the same (stream, hour) loses.
func TestLogHourReStampAndPendingHidden(t *testing.T) {
	db := openCamObserverTestDB(t)

	id, won, err := claimLogHour(db, logHour{StreamID: "ls_1", SiteID: "site_a", HourStart: "2026-07-10T10:00:00Z", TZ: "Asia/Amman"})
	if err != nil || !won {
		t.Fatalf("claim = %v, %v", won, err)
	}
	claimed, _ := getLogHour(db, id)
	if claimed.Status != "pending" || claimed.Seq == 0 {
		t.Fatalf("claimed row = %+v, want pending with a stamped seq", claimed)
	}

	// The duplicate claim loses and consumes nothing.
	maxBefore, _ := camSyncMaxSeq(db)
	if _, won2, err := claimLogHour(db, logHour{StreamID: "ls_1", SiteID: "site_a", HourStart: "2026-07-10T10:00:00Z"}); err != nil || won2 {
		t.Fatalf("duplicate claim = %v, %v; want loss", won2, err)
	}
	if maxAfter, _ := camSyncMaxSeq(db); maxAfter != maxBefore {
		t.Fatalf("lost claim moved the counter %d → %d (rollback must return the seq)", maxBefore, maxAfter)
	}

	// Pending is hidden from the sync cursor.
	if rows, err := listLogHoursAfterSeq(db, "site_a", 0, 0); err != nil || len(rows) != 0 {
		t.Fatalf("sync while pending = %+v, %v; want empty (claims are not content)", rows, err)
	}

	// Terminal update re-stamps: the row reappears past the pre-update cursor
	// exactly once, then the new cursor drains.
	if err := finishLogHour(db, logHour{ID: id, Summary: "ملخص الساعة", Language: "Arabic", EntryCount: 12, GatedCount: 3, Status: "done"}); err != nil {
		t.Fatalf("finishLogHour: %v", err)
	}
	rows, err := listLogHoursAfterSeq(db, "site_a", claimed.Seq, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("sync past pre-update cursor = %+v, %v; want exactly the finished row", rows, err)
	}
	got := rows[0]
	if got.ID != id || got.Seq <= claimed.Seq || got.Status != "done" || got.Summary != "ملخص الساعة" || got.Language != "Arabic" || got.EntryCount != 12 || got.GatedCount != 3 {
		t.Fatalf("finished row = %+v (want same id, larger seq, terminal content)", got)
	}
	if rows, _ := listLogHoursAfterSeq(db, "site_a", got.Seq, 0); len(rows) != 0 {
		t.Fatalf("cursor at the new seq must drain, got %+v", rows)
	}

	// Claim-key getter resolves; a foreign hour does not.
	if h, err := getLogHourByStreamHour(db, "ls_1", "2026-07-10T10:00:00Z"); err != nil || h.ID != id {
		t.Fatalf("getLogHourByStreamHour = %+v, %v", h, err)
	}
	if _, err := getLogHourByStreamHour(db, "ls_1", "2026-07-10T11:00:00Z"); err == nil {
		t.Fatal("unclaimed hour must be ErrNoRows")
	}
}

// TestLogDayClaimFinishRegenerate covers the daily report's lifecycle: claim
// mints a view_token, finish re-stamps seq, and a regeneration (finish again)
// re-stamps AGAIN while preserving id + view_token — the share link survives,
// and Connect sees the same id reappear with fresh content.
func TestLogDayClaimFinishRegenerate(t *testing.T) {
	db := openCamObserverTestDB(t)

	id, won, err := claimLogDay(db, logDay{StreamID: "ls_1", SiteID: "site_a", Day: "2026-07-10"})
	if err != nil || !won {
		t.Fatalf("claim = %v, %v", won, err)
	}
	claimed, _ := getLogDay(db, id)
	if claimed.ViewToken == "" || claimed.Status != "pending" {
		t.Fatalf("claimed day = %+v, want a minted view_token + pending", claimed)
	}
	if _, won2, _ := claimLogDay(db, logDay{StreamID: "ls_1", SiteID: "site_a", Day: "2026-07-10"}); won2 {
		t.Fatal("duplicate day claim must lose")
	}
	// Pending hidden from sync.
	if rows, _ := listLogDaysAfterSeq(db, "site_a", 0, 0); len(rows) != 0 {
		t.Fatalf("sync while pending = %+v, want empty", rows)
	}

	if err := finishLogDay(db, logDay{ID: id, FromTS: "2026-07-09T21:00:00Z", ToTS: "2026-07-10T17:00:00Z",
		Headline: "يوم هادئ", Report: "التقرير اليومي", Language: "Arabic", HoursUsed: 20, EntriesUsed: 240, Status: "done"}); err != nil {
		t.Fatalf("finishLogDay: %v", err)
	}
	done, _ := getLogDay(db, id)
	if done.Seq <= claimed.Seq || done.Status != "done" || done.Headline != "يوم هادئ" {
		t.Fatalf("finished day = %+v", done)
	}
	if done.ViewToken != claimed.ViewToken {
		t.Fatalf("finish changed view_token %q → %q (share links must survive)", claimed.ViewToken, done.ViewToken)
	}

	// Connect consumed up to done.Seq; a regenerate must resurface the SAME row.
	cursor := done.Seq
	if err := finishLogDay(db, logDay{ID: id, FromTS: done.FromTS, ToTS: done.ToTS,
		Headline: "يوم هادئ (منقح)", Report: "نسخة محدثة", Language: "Arabic", HoursUsed: 21, EntriesUsed: 241, Status: "done"}); err != nil {
		t.Fatalf("regenerate finish: %v", err)
	}
	rows, err := listLogDaysAfterSeq(db, "site_a", cursor, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("sync after regenerate = %+v, %v; want the regenerated row once", rows, err)
	}
	regen := rows[0]
	if regen.ID != id || regen.ViewToken != claimed.ViewToken || regen.Headline != "يوم هادئ (منقح)" {
		t.Fatalf("regenerated row = %+v (want same id + view_token, fresh content)", regen)
	}

	// notified_at is delivery bookkeeping — a CAS stamp (exactly one winner,
	// re-fires never) that moves no seq.
	if won, err := claimLogDayNotified(db, id, "2026-07-10T17:05:00Z"); err != nil || !won {
		t.Fatalf("claimLogDayNotified = %v, %v; want the first stamp to win", won, err)
	}
	after, _ := getLogDay(db, id)
	if after.NotifiedAt != "2026-07-10T17:05:00Z" || after.Seq != regen.Seq {
		t.Fatalf("notified stamp = %+v (seq must not move)", after)
	}
	if won, err := claimLogDayNotified(db, id, "2026-07-10T18:00:00Z"); err != nil || won {
		t.Fatalf("second claimLogDayNotified = %v, %v; want lost (already delivered)", won, err)
	}
	if again, _ := getLogDay(db, id); again.NotifiedAt != "2026-07-10T17:05:00Z" {
		t.Fatalf("notified_at moved to %q after a lost CAS", again.NotifiedAt)
	}

	// View-token lookup resolves a real token; blank never resolves.
	if d, err := getLogDayByViewToken(db, claimed.ViewToken); err != nil || d.ID != id {
		t.Fatalf("view-token lookup = %+v, %v", d, err)
	}
	if _, err := getLogDayByViewToken(db, ""); err == nil {
		t.Fatal("empty view_token must never resolve a row")
	}
	if d, err := getLogDayByStreamDay(db, "ls_1", "2026-07-10"); err != nil || d.ID != id {
		t.Fatalf("stream+day lookup = %+v, %v", d, err)
	}
}

// TestLogHourClaimRaceOneWinner races N goroutines at the same (stream, hour)
// claim and requires exactly one winner — the unique claim index under real
// concurrent transactions.
func TestLogHourClaimRaceOneWinner(t *testing.T) {
	db := openCamObserverTestDB(t)

	const n = 8
	var wg sync.WaitGroup
	wins := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, won, err := claimLogHour(db, logHour{StreamID: "ls_race", SiteID: "site_a", HourStart: "2026-07-10T09:00:00Z"})
			if err != nil {
				errs <- err
				return
			}
			if won {
				wins <- id
			}
		}()
	}
	wg.Wait()
	close(wins)
	close(errs)
	for err := range errs {
		t.Fatalf("claim error under race: %v", err)
	}
	var winners []string
	for id := range wins {
		winners = append(winners, id)
	}
	if len(winners) != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", len(winners))
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_log_hours WHERE stream_id = 'ls_race'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows for the raced hour = %d, %v; want 1", count, err)
	}
}

// ─────────────────────────── range + cursor queries ───────────────────────────

func TestLogEntryRangeQueries(t *testing.T) {
	db := openCamObserverTestDB(t)

	seed := []logEntry{
		{StreamID: "ls_1", SiteID: "site_a", FromTS: "2026-07-10T10:00:00Z", ToTS: "2026-07-10T10:05:00Z", CameraIDs: `["cam_1","cam_2"]`, Headline: "one"},
		{StreamID: "ls_1", SiteID: "site_a", FromTS: "2026-07-10T10:05:00Z", ToTS: "2026-07-10T10:10:00Z", CameraIDs: `["cam_1"]`, Headline: "two", MotionGated: true},
		{StreamID: "ls_2", SiteID: "site_a", FromTS: "2026-07-10T10:07:00Z", ToTS: "2026-07-10T10:12:00Z", CameraIDs: `["cam_3"]`, Headline: "three"},
		{StreamID: "ls_9", SiteID: "site_b", FromTS: "2026-07-10T10:08:00Z", ToTS: "2026-07-10T10:13:00Z", CameraIDs: `["cam_1"]`, Headline: "foreign"},
	}
	for _, e := range seed {
		if _, err := insertLogEntry(db, e); err != nil {
			t.Fatalf("seed %q: %v", e.Headline, err)
		}
	}

	// Site browse: newest first, site-scoped.
	got, err := listLogEntriesBySite(db, "site_a", "", "", "", "", 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("site_a entries = %d, %v; want 3", len(got), err)
	}
	if got[0].Headline != "three" || got[2].Headline != "one" {
		t.Fatalf("browse order = [%s %s %s], want newest first", got[0].Headline, got[1].Headline, got[2].Headline)
	}
	if got[1].MotionGated != true {
		t.Fatalf("motion_gated lost in round-trip: %+v", got[1])
	}

	// Stream filter, camera filter (JSON-quoted exact id), window, limit.
	if got, _ = listLogEntriesBySite(db, "site_a", "ls_1", "", "", "", 0); len(got) != 2 {
		t.Fatalf("stream filter = %d rows, want 2", len(got))
	}
	if got, _ = listLogEntriesBySite(db, "site_a", "", "cam_1", "", "", 0); len(got) != 2 {
		t.Fatalf("camera filter = %d rows, want 2 (cam_3 row excluded, site_b row invisible)", len(got))
	}
	if got, _ = listLogEntriesBySite(db, "site_a", "", "cam_", "", "", 0); len(got) != 0 {
		t.Fatalf("a partial camera id must not substring-match, got %d rows", len(got))
	}
	if got, _ = listLogEntriesBySite(db, "site_a", "", "", "2026-07-10T10:05:00Z", "2026-07-10T10:07:00Z", 0); len(got) != 2 {
		t.Fatalf("window filter = %d rows, want 2", len(got))
	}
	if got, _ = listLogEntriesBySite(db, "site_a", "", "", "", "", 1); len(got) != 1 || got[0].Headline != "three" {
		t.Fatalf("limit=1 = %+v, want just the newest", got)
	}

	// Rollup window is [from,to): the entry starting exactly at `to` is excluded.
	rollup, err := listLogEntriesForRollup(db, "ls_1", "2026-07-10T10:00:00Z", "2026-07-10T10:05:00Z")
	if err != nil || len(rollup) != 1 || rollup[0].Headline != "one" {
		t.Fatalf("rollup [from,to) = %+v, %v; want only 'one'", rollup, err)
	}
	rollup, _ = listLogEntriesForRollup(db, "ls_1", "2026-07-10T10:00:00Z", "2026-07-10T11:00:00Z")
	if len(rollup) != 2 || rollup[0].Headline != "one" || rollup[1].Headline != "two" {
		t.Fatalf("rollup order = %+v, want oldest first", rollup)
	}

	// Continuity: newest first, capped.
	recent, err := listRecentLogEntries(db, "ls_1", 1)
	if err != nil || len(recent) != 1 || recent[0].Headline != "two" {
		t.Fatalf("recent = %+v, %v; want the newest ls_1 entry", recent, err)
	}
}

// TestLogSyncCursorCrossSiteIsolation interleaves writes for two sites on the
// shared global counter and proves each site's cursor queries return ONLY its
// own rows (with gaps in seq), paginating cleanly by seq.
func TestLogSyncCursorCrossSiteIsolation(t *testing.T) {
	db := openCamObserverTestDB(t)

	// Interleave: A-entry, B-entry, A-entry, B-hour(done), A-day(done), B-entry.
	if _, err := insertLogEntry(db, logEntry{StreamID: "as", SiteID: "site_a", FromTS: "2026-07-10T10:00:00Z", ToTS: "2026-07-10T10:05:00Z", Headline: "a1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := insertLogEntry(db, logEntry{StreamID: "bs", SiteID: "site_b", FromTS: "2026-07-10T10:00:00Z", ToTS: "2026-07-10T10:05:00Z", Headline: "b1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := insertLogEntry(db, logEntry{StreamID: "as", SiteID: "site_a", FromTS: "2026-07-10T10:05:00Z", ToTS: "2026-07-10T10:10:00Z", Headline: "a2"}); err != nil {
		t.Fatal(err)
	}
	bHourID, won, err := claimLogHour(db, logHour{StreamID: "bs", SiteID: "site_b", HourStart: "2026-07-10T10:00:00Z"})
	if err != nil || !won {
		t.Fatal(err)
	}
	if err := finishLogHour(db, logHour{ID: bHourID, Summary: "b hour", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	aDayID, won, err := claimLogDay(db, logDay{StreamID: "as", SiteID: "site_a", Day: "2026-07-10"})
	if err != nil || !won {
		t.Fatal(err)
	}
	if err := finishLogDay(db, logDay{ID: aDayID, Headline: "a day", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := insertLogEntry(db, logEntry{StreamID: "bs", SiteID: "site_b", FromTS: "2026-07-10T10:05:00Z", ToTS: "2026-07-10T10:10:00Z", Headline: "b2"}); err != nil {
		t.Fatal(err)
	}

	// Site A's entry cursor sees only a1, a2 — never b rows — in seq order.
	aEntries, err := listLogEntriesAfterSeq(db, "site_a", 0, 0)
	if err != nil || len(aEntries) != 2 || aEntries[0].Headline != "a1" || aEntries[1].Headline != "a2" {
		t.Fatalf("site_a entry sync = %+v, %v", aEntries, err)
	}
	if aEntries[1].Seq <= aEntries[0].Seq {
		t.Fatalf("sync order must be seq ASC: %d then %d", aEntries[0].Seq, aEntries[1].Seq)
	}
	// Pagination: resume from the first row's seq (a gap where b1 sits is fine).
	page, _ := listLogEntriesAfterSeq(db, "site_a", aEntries[0].Seq, 1)
	if len(page) != 1 || page[0].Headline != "a2" {
		t.Fatalf("paginated resume = %+v, want a2", page)
	}

	// Hours/days: A sees no hours (B's hour invisible); B sees no days.
	if rows, _ := listLogHoursAfterSeq(db, "site_a", 0, 0); len(rows) != 0 {
		t.Fatalf("site_a hour sync leaked %+v", rows)
	}
	bHours, _ := listLogHoursAfterSeq(db, "site_b", 0, 0)
	if len(bHours) != 1 || bHours[0].ID != bHourID {
		t.Fatalf("site_b hour sync = %+v", bHours)
	}
	if rows, _ := listLogDaysAfterSeq(db, "site_b", 0, 0); len(rows) != 0 {
		t.Fatalf("site_b day sync leaked %+v", rows)
	}
	aDays, _ := listLogDaysAfterSeq(db, "site_a", 0, 0)
	if len(aDays) != 1 || aDays[0].ID != aDayID {
		t.Fatalf("site_a day sync = %+v", aDays)
	}

	// max_seq is the single global watermark both sites compare their cursors to.
	maxSeq, err := camSyncMaxSeq(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range aEntries {
		if e.Seq > maxSeq {
			t.Fatalf("row seq %d exceeds max_seq %d", e.Seq, maxSeq)
		}
	}
	if bHours[0].Seq > maxSeq || aDays[0].Seq > maxSeq {
		t.Fatalf("rollup seqs exceed max_seq %d", maxSeq)
	}
}

// TestLogHourDayBrowseQueries covers the newest-first browse lists for hours
// and days with stream and window filters.
func TestLogHourDayBrowseQueries(t *testing.T) {
	db := openCamObserverTestDB(t)

	for _, hs := range []string{"2026-07-10T08:00:00Z", "2026-07-10T09:00:00Z", "2026-07-10T10:00:00Z"} {
		id, won, err := claimLogHour(db, logHour{StreamID: "ls_1", SiteID: "site_a", HourStart: hs})
		if err != nil || !won {
			t.Fatalf("claim %s: %v", hs, err)
		}
		if err := finishLogHour(db, logHour{ID: id, Summary: "s " + hs, Status: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	otherID, _, _ := claimLogHour(db, logHour{StreamID: "ls_2", SiteID: "site_a", HourStart: "2026-07-10T09:00:00Z"})
	if err := finishLogHour(db, logHour{ID: otherID, Summary: "other stream", Status: "empty"}); err != nil {
		t.Fatal(err)
	}

	hours, err := listLogHoursBySite(db, "site_a", "", "", "", 0)
	if err != nil || len(hours) != 4 {
		t.Fatalf("hours browse = %d, %v; want 4", len(hours), err)
	}
	if hours[0].HourStart != "2026-07-10T10:00:00Z" {
		t.Fatalf("hours order = %+v, want newest first", hours[0])
	}
	if hours, _ = listLogHoursBySite(db, "site_a", "ls_1", "2026-07-10T09:00:00Z", "2026-07-10T09:00:00Z", 0); len(hours) != 1 || hours[0].StreamID != "ls_1" {
		t.Fatalf("hours stream+window filter = %+v", hours)
	}

	for _, day := range []string{"2026-07-08", "2026-07-09", "2026-07-10"} {
		id, won, err := claimLogDay(db, logDay{StreamID: "ls_1", SiteID: "site_a", Day: day})
		if err != nil || !won {
			t.Fatalf("claim day %s: %v", day, err)
		}
		if err := finishLogDay(db, logDay{ID: id, Headline: "d " + day, Status: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	days, err := listLogDaysBySite(db, "site_a", "", "", "", 0)
	if err != nil || len(days) != 3 || days[0].Day != "2026-07-10" {
		t.Fatalf("days browse = %+v, %v; want 3 newest-first", days, err)
	}
	if days, _ = listLogDaysBySite(db, "site_a", "ls_1", "2026-07-09", "2026-07-09", 0); len(days) != 1 || days[0].Day != "2026-07-09" {
		t.Fatalf("days window filter = %+v", days)
	}
	if days, _ = listLogDaysBySite(db, "site_b", "", "", "", 0); len(days) != 0 {
		t.Fatalf("foreign site days = %+v, want none", days)
	}
}
