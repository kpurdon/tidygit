package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStaleWorktreeArchiveRestoresHistoryIndexAndAllFiles(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("safe directory relocation unsupported")
	}
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal index", true: "split index"}[split], func(t *testing.T) {
			f := newCleanupFixture(t)
			t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
			path := filepath.Join(f.root, "old worktree")
			f.branch("old", path)
			f.write(filepath.Join(path, "deleted"), "delete me")
			f.run(path, "add", "deleted")
			f.run(path, "commit", "-m", "unique archived history")
			oid := f.run(path, "rev-parse", "HEAD")
			f.write(filepath.Join(path, "tracked"), "staged version\n")
			f.write(filepath.Join(path, "staged-new"), "new staged bytes\n")
			f.run(path, "add", "tracked", "staged-new")
			f.run(path, "rm", "deleted")
			f.write(filepath.Join(path, "tracked"), "unstaged version\n")
			f.write(filepath.Join(path, "untracked\nfile"), "untracked bytes\n")
			f.write(filepath.Join(path, "ignored"), "ignored secret\n")
			f.write(filepath.Join(f.dir, ".envrc"), "primary environment\n")
			if err := os.Symlink(filepath.Join(f.dir, ".envrc"), filepath.Join(path, ".envrc")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(path, "ignored"), 0o640); err != nil {
				t.Fatal(err)
			}
			if split {
				f.run(path, "update-index", "--split-index")
			}
			status := f.run(path, "status", "--porcelain=v1", "--untracked-files=all")
			staged := f.run(path, "diff", "--cached", "--binary")
			unstaged := f.run(path, "diff", "--binary")
			archive, err := f.repo.archiveStaleWorktree(t.Context(), Worktree{Path: path, Branch: "old", OID: oid}, Branch{Name: "old", OID: oid}, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("original worktree retained: %v", err)
			}
			if got := f.run(f.dir, "worktree", "list", "--porcelain"); strings.Contains(got, path) {
				t.Fatalf("registration retained: %s", got)
			}
			f.run(f.dir, "branch", "-D", "old")
			recovered := filepath.Join(f.root, "recovered")
			f.run(f.root, "init", recovered)
			f.run(recovered, "fetch", filepath.Join(archive, "history.bundle"), "refs/tidygit/*:refs/tidygit/*", "HEAD")
			f.run(recovered, "reset", "--mixed", oid)
			payload := filepath.Join(archive, "files", filepath.Base(path))
			if _, err := os.Lstat(filepath.Join(payload, ".git")); !os.IsNotExist(err) {
				t.Fatalf("active Git pointer retained in archive: %v", err)
			}
			if pointer, err := os.ReadFile(filepath.Join(archive, "gitdir.original")); err != nil || !strings.HasPrefix(string(pointer), "gitdir: ") {
				t.Fatalf("original Git pointer not preserved: %q %v", pointer, err)
			}
			entries, err := os.ReadDir(payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() == ".git" {
					continue
				}
				if err := os.Rename(filepath.Join(payload, entry.Name()), filepath.Join(recovered, entry.Name())); err != nil {
					t.Fatal(err)
				}
			}
			index, err := os.ReadFile(filepath.Join(archive, "index"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(recovered, ".git", "index"), index, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := f.run(recovered, "status", "--porcelain=v1", "--untracked-files=all"); got != status {
				t.Fatalf("status differs:\n%s\nwant:\n%s", got, status)
			}
			if got := f.run(recovered, "diff", "--cached", "--binary"); got != staged {
				t.Fatalf("staged diff lost:\n%s\nwant:\n%s", got, staged)
			}
			if got := f.run(recovered, "diff", "--binary"); got != unstaged {
				t.Fatalf("unstaged diff lost:\n%s\nwant:\n%s", got, unstaged)
			}
			if got := f.run(recovered, "log", "-1", "--format=%s"); got != "unique archived history" {
				t.Fatalf("history lost: %s", got)
			}
			if got, err := os.ReadFile(filepath.Join(recovered, "ignored")); err != nil || string(got) != "ignored secret\n" {
				t.Fatalf("ignored bytes lost: %q %v", got, err)
			}
			if info, err := os.Stat(filepath.Join(recovered, "ignored")); err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("ignored mode lost: %v %v", info, err)
			}
			if link, err := os.Readlink(filepath.Join(recovered, ".envrc")); err != nil || link != filepath.Join(f.dir, ".envrc") {
				t.Fatalf("symlink changed: %q %v", link, err)
			}
			if got, err := os.ReadFile(filepath.Join(f.dir, ".envrc")); err != nil || string(got) != "primary environment\n" {
				t.Fatalf("primary env changed: %q %v", got, err)
			}
		})
	}
}

func TestStaleArchiveRefusesProtectedChangedAndLockedWorktrees(t *testing.T) {
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	oid := f.run(f.dir, "rev-parse", "HEAD")
	if _, err := f.repo.archiveStaleWorktree(t.Context(), Worktree{Path: f.dir, Branch: "main", OID: oid}, Branch{Name: "main", OID: oid}, true); err == nil {
		t.Fatal("archived primary worktree")
	}
	path := filepath.Join(f.root, "stale")
	oid = f.branch("stale", path)
	wt := Worktree{Path: path, Branch: "stale", OID: oid}
	b := Branch{Name: "stale", OID: oid}
	f.run(f.dir, "worktree", "lock", path)
	if _, err := f.repo.archiveStaleWorktree(t.Context(), wt, b, true); err == nil {
		t.Fatal("archived locked worktree")
	}
	f.run(f.dir, "worktree", "unlock", path)
	f.run(path, "commit", "--allow-empty", "-m", "new activity")
	if _, err := f.repo.archiveStaleWorktree(t.Context(), wt, b, true); err == nil {
		t.Fatal("archived changed worktree")
	}
	if _, err := os.Stat(filepath.Join(path, "tracked")); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedBranchRecoversUniqueCommitAfterDeletion(t *testing.T) {
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	f.run(f.dir, "switch", "-c", "old-branch")
	f.write(filepath.Join(f.dir, "unique"), "unpublished bytes\n")
	f.run(f.dir, "add", "unique")
	f.run(f.dir, "commit", "-m", "unpublished history")
	oid := f.run(f.dir, "rev-parse", "HEAD")
	f.run(f.dir, "switch", "main")
	archive, err := f.repo.archiveBranchHistory(t.Context(), Branch{Name: "old-branch", OID: oid})
	if err != nil {
		t.Fatal(err)
	}
	f.assertBranch("old-branch", true)
	f.run(f.dir, "branch", "-D", "old-branch")
	recovered := filepath.Join(f.root, "recovered")
	f.run(f.root, "init", recovered)
	f.run(recovered, "fetch", filepath.Join(archive, "history.bundle"), "refs/heads/old-branch")
	f.run(recovered, "checkout", "--detach", oid)
	if got, err := os.ReadFile(filepath.Join(recovered, "unique")); err != nil || string(got) != "unpublished bytes\n" {
		t.Fatalf("unique history lost: %q %v", got, err)
	}
}

func TestStaleArchiveIndexFailureRetainsOriginalFiles(t *testing.T) {
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	path := filepath.Join(f.root, "old")
	oid := f.branch("old", path)
	linked := f.repo
	linked.Dir = path
	gitDir, err := linked.gitDir(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(gitDir, "index"), "broken index")
	archive, err := f.repo.archiveStaleWorktree(t.Context(), Worktree{Path: path, Branch: "old", OID: oid}, Branch{Name: "old", OID: oid}, true)
	if err == nil {
		t.Fatalf("expected archival refusal, path=%q err=%v", archive, err)
	}
	if got, readErr := os.ReadFile(filepath.Join(path, "tracked")); readErr != nil || string(got) != "base\n" {
		t.Fatalf("original files changed: %q %v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(gitDir, "index.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("our index lock leaked: %v", statErr)
	}
	if !strings.Contains(f.run(f.dir, "worktree", "list", "--porcelain"), "branch refs/heads/old") {
		t.Fatal("original registration removed on failure")
	}
}

func TestStaleArchiveRetainsNestedRepositories(t *testing.T) {
	for _, submodule := range []bool{true, false} {
		t.Run(map[bool]string{true: "submodule", false: "nested repository"}[submodule], func(t *testing.T) {
			f := newCleanupFixture(t)
			t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
			path := filepath.Join(f.root, "old")
			oid := f.branch("old", path)
			nested := filepath.Join(path, "nested")
			if submodule {
				f.run(path, "-c", "protocol.file.allow=always", "submodule", "add", f.dir, "nested")
				childAdmin := f.run(nested, "rev-parse", "--absolute-git-dir")
				f.write(filepath.Join(childAdmin, "objects", "info", "alternates"), filepath.Join(f.dir, ".git", "objects")+"\n")
			} else {
				f.run(path, "init", nested)
			}
			f.write(filepath.Join(nested, "unique"), "nested work\n")
			archive, err := f.repo.archiveStaleWorktree(t.Context(), Worktree{Path: path, Branch: "old", OID: oid}, Branch{Name: "old", OID: oid}, true)
			if err == nil || archive != "" {
				t.Fatalf("nested repository not refused before archival: %q %v", archive, err)
			}
			if got, err := os.ReadFile(filepath.Join(nested, "unique")); err != nil || string(got) != "nested work\n" {
				t.Fatalf("nested work changed: %q %v", got, err)
			}
			if !strings.Contains(f.run(f.dir, "worktree", "list", "--porcelain"), "branch refs/heads/old") {
				t.Fatal("nested repository registration removed")
			}
		})
	}
}
