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

func TestSnapshotPredictsOnlyOwnedCleanup(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "linked")
	oid := f.branch("drop", path)
	f.run(f.dir, "config", "branch.drop.remote", "origin")
	f.run(f.dir, "config", "branch.drop.merge", "refs/heads/drop")
	f.run(f.dir, "config", "branch.drop.extra.remote", "origin")
	include := filepath.Join(f.root, "included-config")
	f.write(include, "[branch \"drop\"]\n\tdescription = included keep\n")
	f.run(f.dir, "config", "include.path", include)
	global := filepath.Join(f.root, "global-config")
	f.write(global, "[branch \"drop\"]\n\tdescription = global keep\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	opts := defaultOptions()
	opts.StaleDays = 0
	baseline, err := localSnapshot(t.Context(), f.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	var removed Worktree
	for _, wt := range baseline.Worktrees {
		if wt.Branch == "drop" {
			removed = wt
		}
	}
	expected, ok := baseline.AfterCleanup([]Branch{{Name: "drop", OID: oid}}, []Worktree{removed})
	if !ok {
		t.Fatal("valid deletion rejected")
	}
	if _, ok := baseline.AfterCleanup([]Branch{{Name: "drop", OID: "changed"}}, nil); ok {
		t.Fatal("wrong branch OID accepted")
	}
	bad := removed
	bad.OID = "changed"
	if _, ok := baseline.AfterCleanup(nil, []Worktree{bad}); ok {
		t.Fatal("wrong worktree OID accepted")
	}
	f.run(f.dir, "worktree", "remove", path)
	if deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "drop", OID: oid}); err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	actual, err := localSnapshot(t.Context(), f.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	if expected != actual.Fingerprint() {
		t.Fatal("owned cleanup fingerprint differs from prediction")
	}
	if baseline.Fingerprint() == expected {
		t.Fatal("prediction mutated baseline or did not change")
	}
	for _, change := range []struct {
		name  string
		apply func()
	}{
		{"branch", func() { f.run(f.dir, "branch", "concurrent") }},
		{"worktree", func() {
			f.run(f.dir, "worktree", "add", "-b", "concurrent-tree", filepath.Join(f.root, "concurrent-tree"))
		}},
		{"file", func() { f.write(filepath.Join(f.dir, "tracked"), "concurrent edit") }},
		{"config", func() { f.run(f.dir, "config", "tidygit.keep", "concurrent") }},
	} {
		change.apply()
		current, err := localSnapshot(t.Context(), f.repo, opts)
		if err != nil {
			t.Fatal(err)
		}
		if current.Fingerprint() == expected {
			t.Fatalf("concurrent %s was marked handled", change.name)
		}
		expected = current.Fingerprint()
	}
}

func TestLocalFingerprintIgnoresBinaryVersion(t *testing.T) {
	f := newCleanupFixture(t)
	opts := defaultOptions()
	opts.StaleDays = 0
	previous := version
	t.Cleanup(func() { version = previous })
	version = "before-ui-change"
	before, err := localFingerprint(t.Context(), f.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	version = "after-ui-change"
	after, err := localFingerprint(t.Context(), f.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("binary version invalidated unchanged local state")
	}
}
