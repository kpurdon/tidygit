package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompletedWorktreeDiscardsIgnoredData(t *testing.T) {
	for _, mode := range []string{"remove", "dry-run", "unique-config", "hidden-index"} {
		t.Run(mode, func(t *testing.T) {
			f := newCleanupFixture(t)
			f.write(filepath.Join(f.dir, ".gitignore"), ".venv/\n.env\n")
			f.run(f.dir, "commit", "-am", "ignore generated files")
			f.run(f.dir, "push")
			f.write(filepath.Join(f.dir, ".env"), "shared configuration\n")
			path := filepath.Join(f.root, "completed")
			oid := f.branch("completed", path)
			if err := os.Mkdir(filepath.Join(path, ".venv"), 0755); err != nil {
				t.Fatal(err)
			}
			f.write(filepath.Join(path, ".venv", "pyvenv.cfg"), "home = /usr/bin\n")
			f.write(filepath.Join(path, ".venv", "generated"), "rebuildable dependency\n")
			f.write(filepath.Join(path, ".env"), "shared configuration\n")
			if mode == "unique-config" {
				f.write(filepath.Join(path, ".env"), "unique local configuration\n")
			}
			if mode == "hidden-index" {
				f.run(path, "update-index", "--assume-unchanged", "tracked")
				f.write(filepath.Join(path, "tracked"), "hidden local changes\n")
			}
			f.pullRequests("MERGED", map[string]string{"completed": oid})
			result := cleanFixture(t, f, mode == "dry-run")
			if mode == "remove" || mode == "unique-config" {
				if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 {
					t.Fatalf("completed disposable checkout retained: %+v", result)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("checkout still exists: %v", err)
				}
			} else {
				if result.WorktreesRemoved != 0 || result.BranchesDeleted != 0 {
					t.Fatalf("protected or preview checkout changed: %+v", result)
				}
				if _, err := os.Stat(filepath.Join(path, ".venv", "generated")); err != nil {
					t.Fatalf("partial disposal before validation: %v", err)
				}
				wantPlanned := 0
				if mode == "dry-run" {
					wantPlanned = 1
				}
				if result.WorktreesPlanned != wantPlanned {
					t.Fatalf("unexpected preview: %+v", result)
				}
			}
			if data, err := os.ReadFile(filepath.Join(f.dir, ".env")); err != nil || string(data) != "shared configuration\n" {
				t.Fatalf("primary config changed: %q %v", data, err)
			}
		})
	}
}

func TestCompletedRecoverableSubmoduleWorktreeRemoved(t *testing.T) {
	f := newCleanupFixture(t)
	f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
	f.run(f.dir, "commit", "-am", "add recoverable child")
	f.run(f.dir, "push")
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	f.run(path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "child")
	f.write(filepath.Join(path, "child", "ignored"), "disposable child environment\n")
	f.pullRequests("MERGED", map[string]string{"completed": oid})
	result := cleanFixture(t, f, false)
	if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 {
		t.Fatalf("recoverable submodule worktree not reclaimed: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("checkout still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "child", "tracked")); err != nil {
		t.Fatalf("primary child changed: %v", err)
	}
}

func TestDisposableWorktreePreservesChildLFSPayload(t *testing.T) {
	f, wt, child := disposableSubmoduleFixture(t)
	payload := "unpublished large object\n"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	f.write(filepath.Join(child, "tracked"), fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", digest, len(payload)))
	f.run(child, "commit", "-am", "published LFS pointer")
	f.run(child, "push", "origin", "HEAD:refs/heads/lfs-pointer")
	f.run(wt.Path, "add", "child")
	f.run(wt.Path, "commit", "-m", "record pointer commit")
	wt.OID = f.run(wt.Path, "rev-parse", "HEAD")
	admin := f.run(child, "rev-parse", "--absolute-git-dir")
	object := filepath.Join(admin, "lfs", "objects", digest[:2], digest[2:4], digest)
	if err := os.MkdirAll(filepath.Dir(object), 0700); err != nil {
		t.Fatal(err)
	}
	f.write(object, payload)
	if err := f.repo.removeDisposableWorktree(t.Context(), wt, Branch{Name: wt.Branch, OID: wt.OID}, true); err == nil {
		t.Fatal("removed checkout containing an unpublished LFS payload")
	}
	if data, err := os.ReadFile(object); err != nil || string(data) != payload {
		t.Fatalf("LFS payload changed: %q, %v", data, err)
	}
}

func TestDisposableWorktreeRechecksEarlierChildHistory(t *testing.T) {
	for _, mutation := range []string{"commit-reset", "stash", "custom-ref"} {
		t.Run(mutation, func(t *testing.T) {
			f, wt, _ := disposableSubmoduleFixture(t)
			f.run(wt.Path, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "other")
			f.run(wt.Path, "commit", "-am", "record second child")
			wt.OID = f.run(wt.Path, "rev-parse", "HEAD")
			first := filepath.Join(f.root, "first-remote")
			done := filepath.Join(f.root, "mutated")
			t.Setenv("TEST_FIRST_REMOTE", first)
			t.Setenv("TEST_REMOTE_MUTATED", done)
			t.Setenv("TEST_CHILD_MUTATION", mutation)
			f.write(filepath.Join(f.root, "bin", "git"), `#!/bin/sh
case " $* " in
  *" ls-remote "*)
    if [ ! -f "$TEST_FIRST_REMOTE" ]; then
      printf '%s' "$PWD" > "$TEST_FIRST_REMOTE"
    elif [ ! -f "$TEST_REMOTE_MUTATED" ]; then
      first=$(cat "$TEST_FIRST_REMOTE")
      initial=$("$TEST_REAL_GIT" -C "$first" rev-parse HEAD)
      case "$TEST_CHILD_MUTATION" in
        commit-reset)
          "$TEST_REAL_GIT" -C "$first" commit --allow-empty -m 'concurrent child history' >/dev/null || exit 1
          "$TEST_REAL_GIT" -C "$first" reset --hard "$initial" >/dev/null || exit 1
          ;;
        stash)
          printf 'concurrent child bytes\n' > "$first/tracked"
          "$TEST_REAL_GIT" -C "$first" stash push >/dev/null || exit 1
          ;;
        custom-ref)
          "$TEST_REAL_GIT" -C "$first" update-ref refs/preserved/concurrent "$initial" || exit 1
          ;;
      esac
      : > "$TEST_REMOTE_MUTATED"
    fi
    ;;
esac
exec "$TEST_REAL_GIT" "$@"
`)
			if err := f.repo.removeDisposableWorktree(t.Context(), wt, Branch{Name: wt.Branch, OID: wt.OID}, true); err == nil {
				t.Fatal("removed checkout after earlier child history changed during remote proof")
			}
			if _, err := os.Stat(done); err != nil {
				t.Fatalf("concurrent mutation did not run: %v", err)
			}
			for _, name := range []string{"tracked", "child/tracked", "other/tracked"} {
				if got, err := os.ReadFile(filepath.Join(wt.Path, name)); err != nil || string(got) != "base\n" {
					t.Fatalf("source changed before all proof checks passed: %s %q %v", name, got, err)
				}
			}
		})
	}
}

func TestDisposableWorktreeIgnoresFsmonitorCache(t *testing.T) {
	f, wt, _ := disposableSubmoduleFixture(t)
	hook := filepath.Join(f.root, "fsmonitor")
	f.write(hook, "#!/bin/sh\nprintf 'unchanged-token\\0'\n")
	if err := os.Chmod(hook, 0755); err != nil {
		t.Fatal(err)
	}
	f.run(wt.Path, "config", "core.fsmonitor", hook)
	f.run(wt.Path, "update-index", "--fsmonitor")
	f.run(wt.Path, "status", "--porcelain")
	f.run(wt.Path, "update-index", "--fsmonitor-valid", "tracked")
	f.write(filepath.Join(wt.Path, "tracked"), "source hidden by stale fsmonitor\n")
	if got := f.run(wt.Path, "status", "--porcelain"); got != "" {
		t.Fatalf("fixture did not hide source changes: %s", got)
	}
	if err := f.repo.removeDisposableWorktree(t.Context(), wt, Branch{Name: wt.Branch, OID: wt.OID}, true); err == nil {
		t.Fatal("removed checkout with source hidden by fsmonitor")
	}
	if got, err := os.ReadFile(filepath.Join(wt.Path, "tracked")); err != nil || string(got) != "source hidden by stale fsmonitor\n" {
		t.Fatalf("source data changed: %q, %v", got, err)
	}
}

func TestDisposableWorktreeRechecksParentIndexVisibility(t *testing.T) {
	f, wt, _ := disposableSubmoduleFixture(t)
	t.Setenv("TEST_PARENT_CHECKOUT", wt.Path)
	f.write(filepath.Join(f.root, "bin", "git"), `#!/bin/sh
case " $* " in
  *" ls-remote "*)
    "$TEST_REAL_GIT" -C "$TEST_PARENT_CHECKOUT" update-index --assume-unchanged tracked || exit 1
    printf 'new hidden source\n' > "$TEST_PARENT_CHECKOUT/tracked"
    ;;
esac
exec "$TEST_REAL_GIT" "$@"
`)
	if err := f.repo.removeDisposableWorktree(t.Context(), wt, Branch{Name: wt.Branch, OID: wt.OID}, true); err == nil {
		t.Fatal("removed checkout after parent index began hiding source changes")
	}
	if got, err := os.ReadFile(filepath.Join(wt.Path, "tracked")); err != nil || string(got) != "new hidden source\n" {
		t.Fatalf("source data changed: %q, %v", got, err)
	}
}

func TestStaleRemovalRechecksActivityAndProtection(t *testing.T) {
	for _, change := range []string{"activity", "locked", "branch", "nested repository", "submodule"} {
		t.Run(change, func(t *testing.T) {
			f := newCleanupFixture(t)
			old := time.Now().Add(-45 * 24 * time.Hour).Truncate(time.Second)
			t.Setenv("GIT_COMMITTER_DATE", old.Format(time.RFC3339))
			t.Setenv("GIT_AUTHOR_DATE", old.Format(time.RFC3339))
			path := filepath.Join(f.root, "stale")
			f.branch("stale", path)
			f.run(path, "commit", "--allow-empty", "-m", "old work")
			oid := f.run(path, "rev-parse", "HEAD")
			wt := Worktree{Path: path, Branch: "stale", OID: oid}
			b := Branch{Name: "stale", OID: oid}
			linked := f.repo
			linked.Dir = path
			admin, err := linked.gitDir(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{path, admin} {
				if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					return os.Chtimes(path, old, old)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if stale, err := staleWorktree(t.Context(), f.repo, wt, 7, time.Now()); err != nil || !stale {
				t.Fatalf("expected stale fixture: %v %v", stale, err)
			}
			switch change {
			case "activity":
				f.write(filepath.Join(path, "ignored"), "recent activity")
			case "locked":
				f.run(f.dir, "worktree", "lock", path)
			case "branch":
				f.run(path, "commit", "--allow-empty", "-m", "changed tip")
			case "nested repository":
				f.run(path, "init", "nested")
			case "submodule":
				f.run(path, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
			}
			if err := f.repo.removeStaleWorktree(t.Context(), wt, b, true, 7); err == nil {
				t.Fatal("changed worktree was removed")
			}
			f.assertBranch("stale", true)
			if _, err := os.Stat(filepath.Join(path, "tracked")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
