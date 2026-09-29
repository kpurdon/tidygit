package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalFingerprintInvalidation(t *testing.T) {
	f := newCleanupFixture(t)
	opts := defaultOptions()
	fingerprint := func() string {
		t.Helper()
		value, err := localFingerprint(t.Context(), f.repo, opts)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	previous := fingerprint()
	assertChanged := func() {
		t.Helper()
		next := fingerprint()
		if next == previous {
			t.Fatal("local change did not invalidate marker")
		}
		previous = next
	}
	if next := fingerprint(); next != previous {
		t.Fatal("unchanged repository fingerprint differs")
	}
	f.run(f.dir, "status", "--porcelain")
	f.run(f.dir, "update-index", "--refresh")
	if next := fingerprint(); next != previous {
		t.Fatal("index stat refresh invalidated fingerprint")
	}
	f.write(filepath.Join(f.dir, "tracked"), "changed content\n")
	assertChanged()
	// Both versions have the same Git status; editing already-dirty files matters.
	f.write(filepath.Join(f.dir, "tracked"), "another dirty version\n")
	assertChanged()
	f.run(f.dir, "add", "tracked")
	assertChanged()
	f.write(filepath.Join(f.dir, ".gitignore"), "cache/\n")
	if err := os.MkdirAll(filepath.Join(f.dir, "cache", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.dir, "cache", "nested", "item"), "first")
	assertChanged()
	f.write(filepath.Join(f.dir, "cache", "nested", "item"), "second version")
	assertChanged()
	f.run(f.dir, "branch", "new-ref")
	assertChanged()
	f.run(f.dir, "worktree", "add", "-b", "linked", filepath.Join(f.root, "linked"))
	assertChanged()
	opts.Auto = true
	assertChanged()
	opts.DryRun = true
	if next := fingerprint(); next != previous {
		t.Fatal("dry-run should inspect same marker")
	}
	opts.PRAuthor = ""
	assertChanged()
	f.write(filepath.Join(f.dir, ".git", "MERGE_HEAD"), "in-progress")
	assertChanged()
}

func TestRunMarkerPersistenceAndReadOnlyMiss(t *testing.T) {
	f := newCleanupFixture(t)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	if match, err := readRunMarker(f.repo, "one"); err != nil || match {
		t.Fatalf("read missing: %v %v", match, err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("read created state directory: %v", err)
	}
	if err := writeRunMarker(f.repo, "one"); err != nil {
		t.Fatal(err)
	}
	if match, err := readRunMarker(f.repo, "one"); err != nil || !match {
		t.Fatalf("read stored: %v %v", match, err)
	}
	if match, err := readRunMarker(f.repo, "two"); err != nil || match {
		t.Fatalf("read stale: %v %v", match, err)
	}
	if err := writeRunMarker(f.repo, "two"); err != nil {
		t.Fatal(err)
	}
	path, _ := runMarkerPath(f.repo)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("marker permissions: %v", info.Mode())
	}
}

func TestLocalFingerprintErrorsAndMetadata(t *testing.T) {
	f := newCleanupFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := localFingerprint(ctx, f.repo, defaultOptions()); err == nil {
		t.Fatal("cancellation ignored")
	}
	linked := filepath.Join(f.root, "linked")
	f.run(f.dir, "worktree", "add", "-b", "linked", linked)
	if err := os.RemoveAll(linked); err != nil {
		t.Fatal(err)
	}
	if _, err := localFingerprint(t.Context(), f.repo, defaultOptions()); err == nil {
		t.Fatal("missing worktree accepted")
	}
}

func TestLocalFingerprintDoesNotFollowSymlinks(t *testing.T) {
	f := newCleanupFixture(t)
	outside := filepath.Join(f.root, "outside")
	f.write(outside, "old")
	if err := os.Symlink(outside, filepath.Join(f.dir, "link")); err != nil {
		t.Fatal(err)
	}
	before, err := localFingerprint(t.Context(), f.repo, defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	f.write(outside, "new content")
	if err := os.Chtimes(outside, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := localFingerprint(t.Context(), f.repo, defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("followed external symlink")
	}
}

func TestLocalFingerprintStableAcrossNoOpGitMaintenance(t *testing.T) {
	f := newCleanupFixture(t)
	// A first fetch may create origin/HEAD; that is a real new local ref.
	f.run(f.dir, "fetch", "--all", "--prune")
	before, err := localFingerprint(t.Context(), f.repo, defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	f.run(f.dir, "fetch", "--all", "--prune")
	f.run(f.dir, "switch", "--no-overwrite-ignore", "main")
	f.run(f.dir, "merge", "--ff-only", "--no-overwrite-ignore", "origin/main")
	f.run(f.dir, "worktree", "prune")
	after, err := localFingerprint(t.Context(), f.repo, defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("no-op maintenance invalidated local marker")
	}
}

func TestInvalidateRunMarker(t *testing.T) {
	f := newCleanupFixture(t)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	if err := invalidateRunMarker(f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("invalidation created state: %v", err)
	}
	if err := writeRunMarker(f.repo, "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if err := invalidateRunMarker(f.repo); err != nil {
		t.Fatal(err)
	}
	if match, err := readRunMarker(f.repo, "fingerprint"); err != nil || match {
		t.Fatalf("stale marker retained: %v %v", match, err)
	}
}
