package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unavailableWorktree(t *testing.T, f *cleanupFixture, kind string) (Worktree, string) {
	t.Helper()
	path := filepath.Join(canonicalPath(f.root), "unavailable")
	oid := f.branch("unavailable", path)
	admin := f.run(path, "rev-parse", "--absolute-git-dir")
	if strings.HasPrefix(kind, "locked") {
		f.run(f.dir, "worktree", "lock", path)
	}
	if strings.HasSuffix(kind, "pointer") {
		if err := os.Remove(filepath.Join(path, ".git")); err != nil {
			t.Fatal(err)
		}
	} else if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	worktrees, err := f.repo.worktrees(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, wt := range worktrees {
		if canonicalPath(wt.Path) == canonicalPath(path) {
			if wt.OID != oid {
				t.Fatal("retained HEAD changed")
			}
			return wt, admin
		}
	}
	t.Fatal("missing retained worktree registration")
	return Worktree{}, ""
}

func TestPreviewContinuesPastUnavailableWorktrees(t *testing.T) {
	for _, kind := range []string{"missing directory", "missing pointer", "locked directory", "locked pointer"} {
		t.Run(kind, func(t *testing.T) {
			f := newCleanupFixture(t)
			wt, admin := unavailableWorktree(t, f, kind)
			path := filepath.Join(f.root, "completed")
			oid := f.branch("completed", path)
			f.pullRequests("MERGED", map[string]string{"completed": oid, "unavailable": wt.OID})
			before := f.run(f.dir, "worktree", "list", "--porcelain")
			index, err := os.ReadFile(filepath.Join(admin, "index"))
			if err != nil {
				t.Fatal(err)
			}
			result := cleanFixture(t, f, true)
			if result.WorktreesPlanned != 1 || result.BranchesPlanned != 1 || result.WorktreesSkipped != 1 {
				t.Fatalf("unavailable registration blocked unrelated cleanup: %+v", result)
			}
			checkCounts(t, result, 2)
			if after := f.run(f.dir, "worktree", "list", "--porcelain"); after != before {
				t.Fatal("preview changed worktree registrations")
			}
			if after, err := os.ReadFile(filepath.Join(admin, "index")); err != nil || string(after) != string(index) {
				t.Fatalf("preview changed retained index: %v", err)
			}
			f.assertBranch("unavailable", true)
			f.assertBranch("completed", true)
		})
	}
}

func TestUnavailableWorktreeRetainsOperationBranches(t *testing.T) {
	for _, operation := range []string{"rebase", "bisect", "malformed rebase"} {
		t.Run(operation, func(t *testing.T) {
			f := newCleanupFixture(t)
			path := filepath.Join(canonicalPath(f.root), "unavailable")
			f.branch("unavailable", path)
			f.run(path, "checkout", "--detach")
			admin := f.run(path, "rev-parse", "--absolute-git-dir")
			oid := f.branch("target", "")
			if operation == "bisect" {
				f.write(filepath.Join(admin, "BISECT_START"), "target\n")
			} else {
				state := filepath.Join(admin, "rebase-merge")
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
				f.write(filepath.Join(state, "head-name"), "refs/heads/unavailable\n")
				if operation == "malformed rebase" {
					f.write(filepath.Join(state, "update-refs"), "refs/heads/target\ntruncated\n")
				} else {
					f.write(filepath.Join(state, "git-rebase-todo"), "update-ref refs/heads/target\n")
				}
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "target", OID: oid})
			if deleted || err == nil {
				t.Fatalf("operation branch lost: deleted=%v err=%v", deleted, err)
			}
			f.assertBranch("target", true)
			if operation != "malformed rebase" {
				unrelated := f.branch("unrelated", "")
				deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "unrelated", OID: unrelated})
				if !deleted || err != nil {
					t.Fatalf("retained operation blocked unrelated branch: %v %v", deleted, err)
				}
			}
		})
	}
}

func TestUnavailableWorktreeSnapshotTracksMetadataAndRestoration(t *testing.T) {
	f := newCleanupFixture(t)
	wt, admin := unavailableWorktree(t, f, "locked directory")
	opts := defaultOptions()
	now := time.Now()
	snapshot := func() string {
		t.Helper()
		fingerprint, err := snapshotWorktree(t.Context(), f.repo, wt, opts, now)
		if err != nil {
			t.Fatal(err)
		}
		return fingerprint
	}
	before := snapshot()
	if got := snapshot(); got != before {
		t.Fatal("unchanged unavailable metadata fingerprint differs")
	}
	f.write(filepath.Join(admin, "BISECT_START"), "unavailable\n")
	changed := snapshot()
	if changed == before {
		t.Fatal("new operation did not invalidate unavailable worktree snapshot")
	}
	if err := os.Remove(filepath.Join(admin, "BISECT_START")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(wt.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(wt.Path, ".git"), "gitdir: "+admin+"\n")
	if restored := snapshot(); restored == before {
		t.Fatal("restored checkout did not invalidate snapshot")
	}
}

func TestCleanupContinuesPastLockedMissingWorktree(t *testing.T) {
	f := newCleanupFixture(t)
	wt, _ := unavailableWorktree(t, f, "locked directory")
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	f.pullRequests("MERGED", map[string]string{"completed": oid, "unavailable": wt.OID})
	result := cleanFixture(t, f, false)
	if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 || result.WorktreesSkipped != 1 {
		t.Fatalf("locked missing registration blocked cleanup: %+v", result)
	}
	f.assertBranch("unavailable", true)
	if result.cacheFingerprint == "" {
		t.Fatal("missing registration prevented successful run marker")
	}
}

func TestUnavailableWorktreeMetadataResolution(t *testing.T) {
	f := newCleanupFixture(t)
	wt, admin := unavailableWorktree(t, f, "missing directory")
	relative, err := filepath.Rel(admin, filepath.Join(wt.Path, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(admin, "gitdir"), relative+"\n")
	got, available, err := f.repo.worktreeGitDir(t.Context(), wt)
	if err != nil || available || got != admin {
		t.Fatalf("relative retained backreference: %q %v %v", got, available, err)
	}
	unregistered := wt
	unregistered.Path = filepath.Join(f.root, "unregistered")
	if _, _, err := f.repo.worktreeGitDir(t.Context(), unregistered); err == nil {
		t.Fatal("unregistered directory accepted as unavailable metadata")
	}
	duplicate := filepath.Join(f.repo.CommonDir, "worktrees", "duplicate")
	if err := os.Mkdir(duplicate, 0o700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(duplicate, "gitdir"), filepath.Join(wt.Path, ".git")+"\n")
	if _, _, err := f.repo.worktreeGitDir(t.Context(), wt); err == nil {
		t.Fatal("ambiguous retained metadata accepted")
	}
}
