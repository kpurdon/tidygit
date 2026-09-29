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
	out, err := r.git(ctx, "log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return time.Time{}, err
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("read worktree commit time: %w", err)
	}
	latest := time.Unix(seconds, 0)
	observe := func(info fs.FileInfo) {
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	err = filepath.WalkDir(wt.Path, func(path string, entry fs.DirEntry, walkErr error) error {
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
	admin, err := r.gitDir(ctx)
	if err != nil {
		return time.Time{}, err
	}
	paths := []string{"HEAD", "logs/HEAD"}
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
		if errors.Is(err, fs.ErrNotExist) && name == "logs/HEAD" {
			continue
		}
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

// staleBranch also considers ref/reflog metadata: a newly created branch at an
// old commit is recent local work, even when its commit timestamp is old.
func staleBranch(ctx context.Context, r repository, b Branch, days int, now time.Time) (bool, error) {
	if days <= 0 {
		return false, nil
	}
	out, err := r.git(ctx, "log", "-1", "--format=%ct", "refs/heads/"+b.Name, "--")
	if err != nil {
		return false, err
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return false, fmt.Errorf("read branch commit time: %w", err)
	}
	latest := time.Unix(seconds, 0)
	found := false
	for _, name := range []string{"logs/refs/heads/" + b.Name, "refs/heads/" + b.Name} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		info, err := os.Lstat(filepath.Join(r.CommonDir, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		found = true
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	if !found {
		// A packed ref without a reflog has no individual creation timestamp;
		// conservatively treat repacking as activity for all such refs.
		info, err := os.Stat(filepath.Join(r.CommonDir, "packed-refs"))
		if err != nil {
			return false, fmt.Errorf("read branch activity metadata: %w", err)
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return activityIsStale(latest, days, now), nil
}
