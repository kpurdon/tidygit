package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// worktreeLastActivity is conservative: file and directory edits, checkout
// history, and the current commit all count, even for ignored files.
func worktreeLastActivity(ctx context.Context, r repository, wt Worktree) (time.Time, error) {
	r.Dir = wt.Path
	var latest time.Time
	observe := func(info fs.FileInfo) {
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	err := filepath.WalkDir(wt.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		observe(info)
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return worktreeMetadataActivity(ctx, r, latest)
}

func worktreeMetadataActivity(ctx context.Context, r repository, latest time.Time) (time.Time, error) {
	out, err := r.git(ctx, "log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return time.Time{}, err
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("read worktree commit time: %w", err)
	}
	if committed := time.Unix(seconds, 0); committed.After(latest) {
		latest = committed
	}
	observe := func(info fs.FileInfo) {
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	admin, err := r.gitDir(ctx)
	if err != nil {
		return time.Time{}, err
	}
	paths := []string{"HEAD"}
	event, err := reflogLastActivity(ctx, r, "HEAD")
	if err != nil {
		return time.Time{}, err
	}
	if event.After(latest) {
		latest = event
	}
	// A clean index can be refreshed by status without user activity. When
	// changes are staged, its timestamp conservatively protects recent staging.
	staged, err := r.git(ctx, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return time.Time{}, err
	}
	if staged != "" {
		paths = append(paths, "index")
	}
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		info, err := os.Lstat(filepath.Join(admin, filepath.FromSlash(name)))
		if err != nil {
			return time.Time{}, err
		}
		observe(info)
	}
	return latest, nil
}

func staleWorktree(ctx context.Context, r repository, wt Worktree, days int, now time.Time) (bool, error) {
	if days <= 0 {
		return false, nil
	}
	last, err := worktreeLastActivity(ctx, r, wt)
	if err != nil {
		return false, err
	}
	return activityIsStale(last, days, now), nil
}

func activityIsStale(last time.Time, days int, now time.Time) bool {
	// Compare whole days rather than multiplying an arbitrary day count into a
	// time.Duration, which can overflow for otherwise valid CLI integers.
	return days > 0 && !last.After(now) && now.Sub(last)/(24*time.Hour) >= time.Duration(days)
}

// staleBranch also considers ref updates and reflog events: a new branch at an
// old commit is recent local work, even when its commit timestamp is old.
func staleBranch(ctx context.Context, r repository, b Branch, days int, now time.Time) (bool, error) {
	if days <= 0 {
		return false, nil
	}
	last, err := branchLastActivity(ctx, r, b)
	return !last.IsZero() && activityIsStale(last, days, now), err
}

func branchLastActivity(ctx context.Context, r repository, b Branch) (time.Time, error) {
	out, err := r.git(ctx, "log", "-1", "--format=%ct", "refs/heads/"+b.Name, "--")
	if err != nil {
		return time.Time{}, err
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("read branch commit time: %w", err)
	}
	latest := time.Unix(seconds, 0)
	event, err := reflogLastActivity(ctx, r, "refs/heads/"+b.Name)
	if err != nil {
		return time.Time{}, err
	}
	if event.After(latest) {
		latest = event
	}
	// Loose refs record creation/update even when reflog recording was disabled.
	// Packing removes loose refs; its global file timestamp is not activity.
	info, err := os.Lstat(filepath.Join(r.CommonDir, "refs", "heads", filepath.FromSlash(b.Name)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, err
	}
	if errors.Is(err, fs.ErrNotExist) && event.IsZero() {
		// Packing can erase the only creation timestamp of a newly created
		// branch with reflogs disabled. Commit age cannot prove branch age.
		return time.Time{}, nil
	}
	if err == nil && info.ModTime().After(latest) {
		latest = info.ModTime()
	}

	return latest, nil
}

func activityDescription(last time.Time, days int, now time.Time) string {
	if days <= 0 {
		return "age-based cleanup disabled (--stale-days 0)"
	}
	if last.IsZero() {
		return fmt.Sprintf("last local activity unknown; cannot establish %dd inactivity", days)
	}
	age := max(time.Duration(0), now.Sub(last)/(24*time.Hour))
	comparison := "below"
	if activityIsStale(last, days, now) {
		comparison = "meets"
	}
	return fmt.Sprintf("last local activity %s (%dd ago; %s %dd threshold)", last.Format("2006-01-02"), age, comparison, days)
}

// Reflog file mtimes change during expiration/repacking. Only event timestamps
// describe user activity; scan all entries in case clocks moved backwards.
func reflogLastActivity(ctx context.Context, r repository, ref string) (time.Time, error) {
	out, err := r.git(ctx, "reflog", "show", "--format=%gD", "--date=unix", ref, "--")
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for line := range strings.SplitSeq(out, "\n") {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		if line == "" {
			continue
		}
		_, stamp, ok := strings.CutLast(line, "@{")
		if !ok || !strings.HasSuffix(stamp, "}") {
			return time.Time{}, fmt.Errorf("invalid reflog timestamp for %s", ref)
		}
		seconds, err := strconv.ParseInt(strings.TrimSuffix(stamp, "}"), 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("read reflog timestamp: %w", err)
		}
		if event := time.Unix(seconds, 0); event.After(latest) {
			latest = event
		}
	}
	return latest, nil
}
