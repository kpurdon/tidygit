package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArchivePreservesOwnedSubmoduleHistoryAndFiles(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("safe directory relocation unsupported")
	}
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	// The primary checkout owns an independent initialized copy that must survive.
	f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "child")
	f.run(f.dir, "commit", "-am", "add child")
	primaryChild := filepath.Join(f.dir, "child")
	primaryOID := f.run(primaryChild, "rev-parse", "HEAD")
	primaryAdmin := f.run(primaryChild, "rev-parse", "--absolute-git-dir")
	f.write(filepath.Join(primaryChild, "ignored"), "primary ignored data\n")
	path := filepath.Join(f.root, "completed")
	f.branch("completed", path)
	f.run(path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "child")
	child := filepath.Join(path, "child")
	f.write(filepath.Join(child, "unique"), "unpublished child commit\n")
	f.run(child, "add", "unique")
	f.run(child, "commit", "-m", "unpublished child history")
	childOID := f.run(child, "rev-parse", "HEAD")
	f.run(path, "add", "child")
	f.run(path, "update-index", "--add", "--cacheinfo", "160000,"+childOID+",empty")
	f.run(path, "commit", "-m", "record unpublished child")
	parentOID := f.run(path, "rev-parse", "HEAD")
	// Keep a reflog-only commit too: a HEAD bundle alone would not preserve it.
	f.run(child, "commit", "--allow-empty", "-m", "reflog-only history")
	unreachable := f.run(child, "rev-parse", "HEAD")
	f.run(child, "reset", "--hard", childOID)
	f.write(filepath.Join(child, "tracked"), "staged child bytes\n")
	f.run(child, "add", "tracked")
	f.write(filepath.Join(child, "tracked"), "unstaged child bytes\n")
	f.write(filepath.Join(child, "untracked"), "untracked child bytes\n")
	f.write(filepath.Join(child, "ignored"), "ignored child bytes\n")
	staged := f.run(child, "diff", "--cached", "--binary")
	unstaged := f.run(child, "diff", "--binary")
	wt := Worktree{Path: path, Branch: "completed", OID: parentOID}
	supported, err := f.repo.submoduleArchivePolicy(t.Context(), wt)
	if err != nil || !supported {
		t.Fatalf("owned submodules refused: %v %v", supported, err)
	}
	archive, err := f.repo.archiveStaleWorktree(t.Context(), wt, Branch{Name: "completed", OID: parentOID}, true)
	if err != nil {
		t.Fatal(err)
	}
	payloadChild := filepath.Join(archive, "files", "completed", "child")
	archivedGit := filepath.Join(archive, "git-admin", "modules", "child")
	runChild := func(args ...string) string {
		return f.run(f.root, append([]string{"--git-dir", archivedGit, "--work-tree", payloadChild}, args...)...)
	}
	if got := runChild("rev-parse", "HEAD"); got != childOID {
		t.Fatalf("child HEAD lost: %s", got)
	}
	if got := runChild("cat-file", "-t", unreachable); got != "commit" {
		t.Fatalf("reflog-only commit lost: %s", got)
	}
	if got := runChild("diff", "--cached", "--binary"); got != staged {
		t.Fatalf("child staged data lost: %s", got)
	}
	if got := runChild("diff", "--binary"); got != unstaged {
		t.Fatalf("child unstaged data lost: %s", got)
	}
	for name, want := range map[string]string{"untracked": "untracked child bytes\n", "ignored": "ignored child bytes\n"} {
		if got, err := os.ReadFile(filepath.Join(payloadChild, name)); err != nil || string(got) != want {
			t.Fatalf("child %s lost: %q %v", name, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(payloadChild, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stale child pointer retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archive, "submodule-gitfiles", "child", "gitdir.original")); err != nil {
		t.Fatalf("original child pointer lost: %v", err)
	}
	for _, name := range []string{"locked", "index.lock"} {
		if _, err := os.Stat(filepath.Join(archive, "git-admin", name)); !os.IsNotExist(err) {
			t.Fatalf("owned lock leaked: %s %v", name, err)
		}
	}
	if listing := f.run(f.dir, "worktree", "list", "--porcelain"); strings.Contains(listing, "branch refs/heads/completed") {
		t.Fatalf("registration retained: %s", listing)
	}
	if got := f.run(primaryChild, "rev-parse", "HEAD"); got != primaryOID {
		t.Fatalf("primary child HEAD changed: %s", got)
	}
	if got := f.run(primaryChild, "rev-parse", "--absolute-git-dir"); got != primaryAdmin {
		t.Fatalf("primary child storage changed: %s", got)
	}
	if got, err := os.ReadFile(filepath.Join(primaryChild, "ignored")); err != nil || string(got) != "primary ignored data\n" {
		t.Fatalf("primary child files changed: %q %v", got, err)
	}
}

func TestSubmodulePolicyRejectsSharedExternalAndPromisorStorage(t *testing.T) {
	for _, kind := range []string{"alternates", "promisor", "shallow", "linked-worktree"} {
		t.Run(kind, func(t *testing.T) {
			f := newCleanupFixture(t)
			path := filepath.Join(f.root, "completed")
			oid := f.branch("completed", path)
			f.run(path, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "child")
			child := filepath.Join(path, "child")
			admin := f.run(child, "rev-parse", "--absolute-git-dir")
			switch kind {
			case "alternates":
				f.write(filepath.Join(admin, "objects", "info", "alternates"), filepath.Join(f.dir, ".git", "objects")+"\n")
			case "promisor":
				f.run(child, "config", "remote.origin.promisor", "true")
			case "shallow":
				f.write(filepath.Join(admin, "shallow"), oid+"\n")
			case "linked-worktree":
				f.run(child, "worktree", "add", "--detach", filepath.Join(f.root, "other-child"))
			}
			supported, err := f.repo.submoduleArchivePolicy(t.Context(), Worktree{Path: path, Branch: "completed", OID: oid})
			if err == nil || !supported {
				t.Fatalf("unsupported storage not identified: %v %v", supported, err)
			}
			if _, err := os.Stat(filepath.Join(child, "tracked")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletedSubmoduleCleanupArchivesIgnoredFilesAndKeepsDirtyChildren(t *testing.T) {
	for _, mode := range []string{"clean", "dirty", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f := newCleanupFixture(t)
			t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
			f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "child")
			f.run(f.dir, "commit", "-am", "add child")
			primaryChild := filepath.Join(f.dir, "child")
			f.write(filepath.Join(primaryChild, "ignored"), "primary data\n")
			path := filepath.Join(f.root, "completed")
			oid := f.branch("completed", path)
			f.run(path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "child")
			child := filepath.Join(path, "child")
			f.write(filepath.Join(child, "ignored"), "child ignored data\n")
			if mode == "dirty" {
				f.write(filepath.Join(child, "tracked"), "active child work\n")
			}
			f.pullRequests("MERGED", map[string]string{"completed": oid})
			result := cleanFixture(t, f, mode == "dry-run")
			if mode == "clean" {
				if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 || len(result.Archives) != 1 {
					t.Fatalf("clean submodule cleanup failed: %+v", result)
				}
				if got, err := os.ReadFile(filepath.Join(result.Archives[0], "files", "completed", "child", "ignored")); err != nil || string(got) != "child ignored data\n" {
					t.Fatalf("ignored child file lost: %q %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(result.Archives[0], "git-admin", "modules", "child", "HEAD")); err != nil {
					t.Fatalf("child metadata lost: %v", err)
				}
			} else {
				if result.WorktreesRemoved != 0 || result.BranchesDeleted != 0 || len(result.Archives) != 0 {
					t.Fatalf("protected submodule changed: %+v", result)
				}
				if _, err := os.Stat(filepath.Join(child, "ignored")); err != nil {
					t.Fatal(err)
				}
				f.assertBranch("completed", true)
				if mode == "dry-run" && result.WorktreesPlanned != 1 {
					t.Fatalf("submodule archive not previewed: %+v", result)
				}
			}
			if got, err := os.ReadFile(filepath.Join(primaryChild, "ignored")); err != nil || string(got) != "primary data\n" {
				t.Fatalf("primary module changed: %q %v", got, err)
			}
		})
	}
}

func TestSubmodulePolicyChecksDeinitializedMetadata(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	f.run(path, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "child")
	child := filepath.Join(path, "child")
	admin := f.run(child, "rev-parse", "--absolute-git-dir")
	f.run(path, "submodule", "deinit", "--force", "child")
	f.write(filepath.Join(admin, "objects", "info", "alternates"), filepath.Join(f.dir, ".git", "objects")+"\n")
	supported, err := f.repo.submoduleArchivePolicy(t.Context(), Worktree{Path: path, Branch: "completed", OID: oid})
	if err == nil || !supported {
		t.Fatalf("deinitialized external object storage accepted: %v %v", supported, err)
	}
}

func TestArchivePreservesRemovedSubmoduleMetadata(t *testing.T) {
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	path := filepath.Join(f.root, "completed")
	f.branch("completed", path)
	f.run(path, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "child")
	f.run(path, "commit", "-am", "add child")
	childOID := f.run(filepath.Join(path, "child"), "rev-parse", "HEAD")
	f.run(path, "rm", "--force", "child")
	f.run(path, "commit", "-am", "remove child")
	oid := f.run(path, "rev-parse", "HEAD")
	wt := Worktree{Path: path, Branch: "completed", OID: oid}
	hasModules, err := f.repo.submoduleArchivePolicy(t.Context(), wt)
	if err != nil || !hasModules {
		t.Fatalf("removed child storage not classified: %v %v", hasModules, err)
	}
	archive, err := f.repo.archiveStaleWorktree(t.Context(), wt, Branch{Name: "completed", OID: oid}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.run(f.root, "--git-dir", filepath.Join(archive, "git-admin", "modules", "child"), "--work-tree", f.root, "rev-parse", "HEAD"); got != childOID {
		t.Fatalf("removed child history lost: %s", got)
	}
}

func TestSubmodulePolicyRejectsMetadataForAnotherCheckout(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	f.run(path, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "child")
	child := filepath.Join(path, "child")
	admin := f.run(child, "rev-parse", "--absolute-git-dir")
	f.run(child, "config", "core.worktree", f.dir)
	hasModules, err := f.repo.submoduleArchivePolicy(t.Context(), Worktree{Path: path, Branch: "completed", OID: oid})
	if err == nil || !hasModules {
		t.Fatalf("external checkout metadata accepted: %v %v (Git directory %s)", hasModules, err, admin)
	}
}

func TestSubmoduleArchiveFailureLocksMetadataAgainstPruning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	path := filepath.Join(f.root, "completed")
	f.branch("completed", path)
	f.run(path, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "child")
	f.run(path, "commit", "-am", "add child")
	oid := f.run(path, "rev-parse", "HEAD")
	wt := Worktree{Path: path, Branch: "completed", OID: oid}
	admin, err := f.repo.ownedWorktreeAdmin(t.Context(), wt)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(admin)
	if err = os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	archive, err := f.repo.archiveStaleWorktree(t.Context(), wt, Branch{Name: "completed", OID: oid}, true)
	if err == nil || archive == "" {
		t.Fatalf("expected partial archival failure: %q %v", archive, err)
	}
	if _, err := os.Stat(filepath.Join(archive, "files", "completed", "child", "tracked")); err != nil {
		t.Fatalf("worktree files not recoverable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(admin, "locked")); err != nil {
		t.Fatalf("metadata not protected against pruning: %v", err)
	}
	if _, err := os.Stat(filepath.Join(admin, "modules", "child", "HEAD")); err != nil {
		t.Fatalf("original child metadata not recoverable: %v", err)
	}
	f.assertBranch("completed", true)
}
