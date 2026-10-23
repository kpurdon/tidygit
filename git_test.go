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
	f.pullRequests("MERGED", map[string]string{"merged": oid})
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

func caseAliasedWorktree(t *testing.T) (*cleanupFixture, string, string, string) {
	t.Helper()
	f := newCleanupFixture(t)
	name := "infra-5953/foo"
	path := filepath.Join(f.root, "aliased")
	oid := f.branch(name, path)
	lower, err := os.Stat(filepath.Join(f.repo.CommonDir, "refs", "heads", name))
	if err != nil {
		t.Fatal(err)
	}
	upper, err := os.Stat(filepath.Join(f.repo.CommonDir, "refs", "heads", "INFRA-5953/foo"))
	if os.IsNotExist(err) {
		t.Skip("filesystem does not alias differently cased loose refs")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(lower, upper) {
		t.Skip("differently cased loose refs identify distinct files")
	}
	f.run(path, "symbolic-ref", "HEAD", "refs/heads/INFRA-5953/foo")
	if got := f.run(path, "rev-parse", "HEAD"); got != oid {
		t.Fatalf("case alias resolves to %s, want %s", got, oid)
	}
	return f, path, name, oid
}

func TestDeleteBranchRefusesCaseAliasedWorktree(t *testing.T) {
	f, path, name, oid := caseAliasedWorktree(t)
	deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: name, OID: oid})
	if deleted || err == nil {
		t.Fatalf("deleted a checked-out loose-ref alias: deleted=%v err=%v", deleted, err)
	}
	f.assertBranch(name, true)
	if got := f.run(path, "rev-parse", "HEAD"); got != oid {
		t.Fatalf("worktree HEAD changed: got %s want %s", got, oid)
	}
}

func TestDeleteBranchKeepsCaseDistinctPackedRefsIndependent(t *testing.T) {
	f := newCleanupFixture(t)
	oid := f.branch("topic", "")
	f.run(f.dir, "pack-refs", "--all", "--prune")
	path := filepath.Join(f.root, "upper")
	f.branch("TOPIC", path)
	f.run(f.dir, "pack-refs", "--all", "--prune")
	for _, name := range []string{"topic", "TOPIC"} {
		if _, err := os.Stat(filepath.Join(f.repo.CommonDir, "refs", "heads", name)); !os.IsNotExist(err) {
			t.Fatalf("expected packed-only ref %s: %v", name, err)
		}
		f.assertBranch(name, true)
	}
	deleted, err := f.repo.deleteBranch(t.Context(), Branch{Name: "topic", OID: oid})
	if !deleted || err != nil {
		t.Fatalf("distinct packed branch was treated as checked out: deleted=%v err=%v", deleted, err)
	}
	f.assertBranch("topic", false)
	f.assertBranch("TOPIC", true)
	if got := f.run(path, "rev-parse", "HEAD"); got != oid {
		t.Fatalf("checked-out packed ref changed: got %s want %s", got, oid)
	}
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

func TestInProgressReadsFreshMetadataWithoutGit(t *testing.T) {
	for _, layout := range []string{"directory", "absolute file", "relative file", "symlink directory", "symlink file", "symlink parent"} {
		t.Run(layout, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			t.Setenv("GIT_DIR", "")
			t.Setenv("GIT_WORK_TREE", "")
			root := t.TempDir()
			worktree := filepath.Join(root, "worktree")
			if err := os.Mkdir(worktree, 0o700); err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(root, "metadata\nwith spaces ")
			if layout == "directory" {
				metadata = filepath.Join(worktree, ".git")
			}
			if err := os.Mkdir(metadata, 0o700); err != nil {
				t.Fatal(err)
			}
			dotgit := filepath.Join(worktree, ".git")
			switch layout {
			case "absolute file", "relative file", "symlink file", "symlink parent":
				target := metadata
				if layout != "absolute file" {
					var err error
					target, err = filepath.Rel(worktree, metadata)
					if err != nil {
						t.Fatal(err)
					}
				}
				if layout == "symlink parent" {
					child := filepath.Join(metadata, "child")
					if err := os.Mkdir(child, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(child, filepath.Join(worktree, "link")); err != nil {
						t.Fatal(err)
					}
					target = "link/.."
				}
				file := dotgit
				if layout == "symlink file" {
					file = filepath.Join(root, "gitfile")
				}
				if err := os.WriteFile(file, []byte("gitdir: "+target+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if file != dotgit {
					if err := os.Symlink(file, dotgit); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink directory":
				if err := os.Symlink(metadata, dotgit); err != nil {
					t.Fatal(err)
				}
			}
			r := repository{Dir: worktree}
			check := func(want string) {
				t.Helper()
				if got, err := r.inProgress(t.Context()); err != nil || got != want {
					t.Fatalf("operation state = %q, %v; want %q", got, err, want)
				}
			}
			check("")
			marker := filepath.Join(metadata, "rebase-merge")
			if err := os.Mkdir(marker, 0o700); err != nil {
				t.Fatal(err)
			}
			check("rebase-merge")
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			check("")
		})
	}
}

func TestInProgressRejectsMissingGitdir(t *testing.T) {
	r := repository{Dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(r.Dir, ".git"), []byte("gitdir: missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := r.inProgress(t.Context()); err == nil {
		t.Fatalf("missing metadata accepted as safe: %q", state)
	}
}
