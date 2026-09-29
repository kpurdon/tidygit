package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewlineWorktreePathDoesNotTargetAnotherWorktree(t *testing.T) {
	f := newCleanupFixture(t)
	safe := filepath.Join(f.root, "victim")
	unusual := safe + "\nworktree " + safe
	oid := f.branch("merged", unusual)
	f.branch("unmerged", safe)
	f.merged(map[string]string{"merged": oid})
	r := cleanFixture(t, f, false)
	checkCounts(t, r, 2)
	if r.WorktreesRemoved != 1 {
		t.Fatalf("expected one worktree removed: %+v", r)
	}
	if _, err := os.Stat(unusual); !os.IsNotExist(err) {
		t.Fatalf("merged newline path still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(safe, "tracked")); err != nil {
		t.Fatalf("wrong worktree removed: %v", err)
	}
	f.assertBranch("merged", false)
	f.assertBranch("unmerged", true)
}

func TestDeleteBranchRefusesMovedOrCheckedOutBranch(t *testing.T) {
	f := newCleanupFixture(t)
	oid := f.branch("moved", "")
	f.run(f.dir, "commit", "--allow-empty", "-m", "advance")
	f.run(f.dir, "branch", "-f", "moved", "HEAD")
	if _, err := f.repo.deleteBranch(t.Context(), Branch{Name: "moved", OID: oid}); err == nil {
		t.Fatal("deleted a branch moved since discovery")
	}
	f.assertBranch("moved", true)
	oid = f.run(f.dir, "rev-parse", "main")
	if _, err := f.repo.deleteBranch(t.Context(), Branch{Name: "main", OID: oid}); err == nil {
		t.Fatal("deleted checked-out branch")
	}
	f.assertBranch("main", true)
}

func TestDeleteBranchAtomicallyPreservesConcurrentUpdate(t *testing.T) {
	f := newCleanupFixture(t)
	original := f.branch("racing", "")
	f.run(f.dir, "commit", "--allow-empty", "-m", "new work")
	advanced := f.run(f.dir, "rev-parse", "HEAD")
	t.Setenv("TEST_NEW_TIP", advanced)
	f.write(filepath.Join(f.root, "bin", "git"), `#!/bin/sh
if [ "$1" = update-ref ]; then
 for arg do
  if [ "$arg" = refs/heads/racing ]; then
   "$TEST_REAL_GIT" update-ref refs/heads/racing "$TEST_NEW_TIP" || exit
   break
  fi
 done
fi
exec "$TEST_REAL_GIT" "$@"
`)
	deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "racing", OID: original})
	if deleted || err == nil {
		t.Fatalf("concurrently advanced branch deletion: deleted=%v err=%v", deleted, err)
	}
	if got := f.run(f.dir, "rev-parse", "racing"); got != advanced {
		t.Fatalf("new branch tip lost: got %s want %s", got, advanced)
	}
}

func TestDeleteBranchRemovesItsUpstreamConfiguration(t *testing.T) {
	f := newCleanupFixture(t)
	oid := f.branch("finished", "")
	f.run(f.dir, "branch", "--set-upstream-to=origin/main", "finished")
	deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "finished", OID: oid})
	if !deleted || err != nil {
		t.Fatalf("delete configured branch: deleted=%v err=%v", deleted, err)
	}
	f.assertBranch("finished", false)
	config := f.run(f.dir, "config", "--local", "--list")
	if strings.Contains(config, "branch.finished.") {
		t.Fatalf("deleted branch configuration retained: %s", config)
	}
	if !strings.Contains(config, "branch.main.remote=origin") {
		t.Fatal("unrelated upstream configuration removed")
	}
}

func TestDeleteSymbolicBranchDoesNotDeleteTarget(t *testing.T) {
	f := newCleanupFixture(t)
	oid := f.run(f.dir, "rev-parse", "main")
	f.run(f.dir, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
	deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "alias", OID: oid})
	if !deleted || err != nil {
		t.Fatalf("delete alias: deleted=%v err=%v", deleted, err)
	}
	f.assertBranch("alias", false)
	if got := f.run(f.dir, "rev-parse", "main"); got != oid {
		t.Fatalf("symbolic ref target changed: %s", got)
	}
}

func TestDeleteBranchPreservesDetachedRebaseWork(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "rebasing")
	f.branch("rebasing", path)
	f.write(filepath.Join(path, "tracked"), "feature\n")
	f.run(path, "commit", "-am", "feature change")
	oid := f.run(path, "rev-parse", "HEAD")
	f.write(filepath.Join(f.dir, "tracked"), "main\n")
	f.run(f.dir, "commit", "-am", "main change")
	cmd := exec.Command(f.git, "rebase", "main")
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected real rebase conflict: %s", out)
	}
	worktreePath := f.run(f.dir, "for-each-ref", "--format=%(worktreepath)", "refs/heads/rebasing")
	t.Logf("Git worktreepath during detached rebase: %q", worktreePath)
	deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "rebasing", OID: oid})
	if deleted || err == nil {
		t.Fatalf("deleted branch participating in detached rebase: deleted=%v err=%v", deleted, err)
	}
	f.assertBranch("rebasing", true)
	state := f.run(path, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge")
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("rebase state missing: %v", err)
	}
}
