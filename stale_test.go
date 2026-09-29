package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func ageFixture(t *testing.T, f *cleanupFixture, when time.Time) {
	t.Helper()
	t.Setenv("GIT_COMMITTER_DATE", when.Format(time.RFC3339))
	t.Setenv("GIT_AUTHOR_DATE", when.Format(time.RFC3339))
	f.run(f.dir, "commit", "--amend", "--no-edit", "--date", when.Format(time.RFC3339))
	if err := filepath.WalkDir(f.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, when, when)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStaleWorktreeAgeAndRecentIgnoredActivity(t *testing.T) {
	f := newCleanupFixture(t)
	now := time.Now().Truncate(time.Second)
	old := now.Add(-31 * 24 * time.Hour)
	ageFixture(t, f, old)
	wt := Worktree{Path: f.dir}
	stale, err := staleWorktree(t.Context(), f.repo, wt, 30, now)
	if err != nil || !stale {
		t.Fatalf("old worktree: stale=%v err=%v", stale, err)
	}
	f.write(filepath.Join(f.dir, "ignored"), "recent ignored work")
	stale, err = staleWorktree(t.Context(), f.repo, wt, 30, now.Add(time.Second))
	if err != nil || stale {
		t.Fatalf("active ignored file: stale=%v err=%v", stale, err)
	}
	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	stale, err = staleWorktree(t.Context(), f.repo, wt, 30, now)
	if err == nil || stale {
		t.Fatalf("missing worktree must retain: %v %v", stale, err)
	}
	stale, err = staleWorktree(t.Context(), f.repo, wt, 0, now)
	if err != nil || stale {
		t.Fatalf("disabled must skip scan: %v %v", stale, err)
	}
}

func TestStaleWorktreeIgnoresCleanIndexRefresh(t *testing.T) {
	f := newCleanupFixture(t)
	now := time.Now().Truncate(time.Second)
	ageFixture(t, f, now.Add(-31*24*time.Hour))
	if err := os.Chtimes(filepath.Join(f.dir, ".git", "index"), now, now); err != nil {
		t.Fatal(err)
	}
	stale, err := staleWorktree(t.Context(), f.repo, Worktree{Path: f.dir}, 30, now)
	if err != nil || !stale {
		t.Fatalf("clean index refresh counts as activity: %v %v", stale, err)
	}
	f.write(filepath.Join(f.dir, "tracked"), "staged work")
	f.run(f.dir, "add", "tracked")
	// Backdate the file/root; staging metadata still protects recent index edits.
	old := now.Add(-31 * 24 * time.Hour)
	for _, p := range []string{f.dir, filepath.Join(f.dir, "tracked")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	stale, err = staleWorktree(t.Context(), f.repo, Worktree{Path: f.dir}, 30, now.Add(time.Second))
	if err != nil || stale {
		t.Fatalf("recent staged work unprotected: %v %v", stale, err)
	}
}

func TestActivityAgeBoundary(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		age   time.Duration
		days  int
		stale bool
	}{{30 * 24 * time.Hour, 30, true}, {30*24*time.Hour - time.Nanosecond, 30, false}, {31 * 24 * time.Hour, 0, false}, {-time.Second, 30, false}} {
		if got := activityIsStale(now.Add(-tc.age), tc.days, now); got != tc.stale {
			t.Errorf("age=%v days=%d: %v", tc.age, tc.days, got)
		}
	}
}

func TestLocalMarkerInvalidatesAtStaleThreshold(t *testing.T) {
	f := newCleanupFixture(t)
	old := time.Now().Add(-31 * 24 * time.Hour).Truncate(time.Second)
	ageFixture(t, f, old)
	opts := defaultOptions()
	opts.StaleDays = 30
	before, err := localFingerprintAt(t.Context(), f.repo, opts, old.Add(29*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	same, err := localFingerprintAt(t.Context(), f.repo, opts, old.Add(29*24*time.Hour+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if before != same {
		t.Fatal("marker expires before threshold")
	}
	after, err := localFingerprintAt(t.Context(), f.repo, opts, old.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("threshold crossing did not invalidate marker")
	}
}

func TestNewWorktreeWithOldCommitIsNotStale(t *testing.T) {
	f := newCleanupFixture(t)
	now := time.Now().Truncate(time.Second)
	ageFixture(t, f, now.Add(-90*24*time.Hour))
	linked := filepath.Join(f.root, "new-linked")
	f.run(f.dir, "worktree", "add", "-b", "new-linked", linked)
	stale, err := staleWorktree(t.Context(), f.repo, Worktree{Path: linked}, 30, now.Add(time.Second))
	if err != nil || stale {
		t.Fatalf("new worktree considered stale: %v %v", stale, err)
	}
}

func TestStaleBranchProtectsNewRefAtOldCommit(t *testing.T) {
	f := newCleanupFixture(t)
	now := time.Now().Truncate(time.Second)
	old := now.Add(-31 * 24 * time.Hour)
	ageFixture(t, f, old)
	f.run(f.dir, "branch", "snapshot")
	branch := Branch{Name: "snapshot"}
	stale, err := staleBranch(t.Context(), f.repo, branch, 30, now.Add(time.Second))
	if err != nil || stale {
		t.Fatalf("new branch at old commit: %v %v", stale, err)
	}
	for _, name := range []string{"refs/heads/snapshot", "logs/refs/heads/snapshot"} {
		if err := os.Chtimes(filepath.Join(f.repo.CommonDir, filepath.FromSlash(name)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	stale, err = staleBranch(t.Context(), f.repo, branch, 30, now)
	if err != nil || !stale {
		t.Fatalf("old branch: %v %v", stale, err)
	}
	// Keep the primary worktree recent so only the standalone branch crosses.
	if err := os.Chtimes(filepath.Join(f.dir, "tracked"), now, now); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.StaleDays = 30
	before, err := localFingerprintAt(t.Context(), f.repo, opts, old.Add(29*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	after, err := localFingerprintAt(t.Context(), f.repo, opts, old.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("branch age threshold did not invalidate marker")
	}
}
