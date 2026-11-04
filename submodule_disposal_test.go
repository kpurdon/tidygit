package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func disposableSubmoduleFixture(t *testing.T) (*cleanupFixture, Worktree, string) {
	t.Helper()
	f := newCleanupFixture(t)
	f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
	f.run(f.dir, "commit", "-am", "record child")
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	f.run(path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "child")
	return f, Worktree{Path: path, Branch: "completed", OID: oid}, filepath.Join(path, "child")
}

func TestDisposableSubmodulesReadOnlyRecoveryProof(t *testing.T) {
	f, wt, child := disposableSubmoduleFixture(t)
	admin := f.run(child, "rev-parse", "--absolute-git-dir")
	before, err := os.ReadFile(filepath.Join(admin, "index"))
	if err != nil {
		t.Fatal(err)
	}
	refs := f.run(child, "show-ref")
	plan, err := f.repo.disposableSubmodulePlan(t.Context(), wt)
	if err != nil || len(plan.modules) != 1 {
		t.Fatalf("fully recoverable child rejected: %+v, %v", plan, err)
	}
	after, err := os.ReadFile(filepath.Join(admin, "index"))
	if err != nil || !bytes.Equal(before, after) || f.run(child, "show-ref") != refs {
		t.Fatalf("proof changed index or refs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(admin, "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("proof fetched remote history: %v", err)
	}
	if got := f.run(child, "status", "--porcelain"); got != "" {
		t.Fatalf("proof changed child files: %s", got)
	}
}

func TestDisposableSubmodulesKeepUnrecoverableState(t *testing.T) {
	for _, kind := range []string{"unique-branch", "broken-ref", "unique-commit", "dangling-blob", "reflog-only", "stash", "custom-ref", "tag", "dirty", "untracked", "assume-unchanged", "skip-worktree", "missing-remote", "gitlink-mismatch", "operation", "deinitialized", "removed"} {
		t.Run(kind, func(t *testing.T) {
			f, wt, child := disposableSubmoduleFixture(t)
			initial := f.run(child, "rev-parse", "HEAD")
			switch kind {
			case "unique-branch":
				f.run(child, "branch", "local-only")
			case "broken-ref":
				admin := f.run(child, "rev-parse", "--absolute-git-dir")
				f.write(filepath.Join(admin, "refs", "heads", "broken"), "1234567890123456789012345678901234567890\n")
			case "unique-commit", "reflog-only", "gitlink-mismatch":
				f.run(child, "commit", "--allow-empty", "-m", "unpublished child work")
				if kind == "reflog-only" {
					f.run(child, "reset", "--hard", initial)
				} else if kind == "unique-commit" {
					f.run(wt.Path, "add", "child")
					f.run(wt.Path, "commit", "-m", "record unpublished child")
				}
			case "dangling-blob":
				path := filepath.Join(f.root, "unpublished")
				f.write(path, "not in any ref or reflog\n")
				f.run(child, "hash-object", "-w", path)
			case "stash":
				f.write(filepath.Join(child, "tracked"), "stashed child edit\n")
				f.run(child, "stash", "push")
			case "custom-ref":
				f.run(child, "update-ref", "refs/preserved/local", initial)
			case "tag":
				f.run(child, "tag", "local-only")
			case "dirty", "untracked":
				name := "tracked"
				if kind == "untracked" {
					name = "untracked"
				}
				f.write(filepath.Join(child, name), "local work\n")
			case "assume-unchanged", "skip-worktree":
				f.run(child, "update-index", "--"+kind, "tracked")
				f.write(filepath.Join(child, "tracked"), "hidden local work\n")
			case "missing-remote":
				f.run(child, "remote", "remove", "origin")
			case "operation":
				admin := f.run(child, "rev-parse", "--absolute-git-dir")
				f.write(filepath.Join(admin, "CHERRY_PICK_HEAD"), initial+"\n")
			case "deinitialized":
				f.run(wt.Path, "submodule", "deinit", "--all")
			case "removed":
				f.run(wt.Path, "rm", "child")
				f.run(wt.Path, "commit", "-m", "remove child")
			}
			wt.OID = f.run(wt.Path, "rev-parse", "HEAD")
			if err := f.repo.checkDisposableSubmodules(t.Context(), wt); err == nil {
				t.Fatalf("%s child state was accepted for disposal", kind)
			}
			if _, err := os.Stat(wt.Path); err != nil {
				t.Fatalf("read-only proof removed worktree: %v", err)
			}
		})
	}
}

func TestDisposableSubmodulesRemoteRefreshAndAnnotatedTags(t *testing.T) {
	f, wt, child := disposableSubmoduleFixture(t)
	f.run(child, "tag", "-a", "published", "-m", "published annotation")
	f.run(child, "push", "origin", "refs/tags/published")
	if err := f.repo.checkDisposableSubmodules(t.Context(), wt); err != nil {
		t.Fatalf("published annotated tag rejected: %v", err)
	}
	f.run(child, "push", "origin", ":refs/tags/published")
	if err := f.repo.checkDisposableSubmodules(t.Context(), wt); err == nil {
		t.Fatal("proof reused stale remote advertisement after tag deletion")
	}
}

func TestDisposableSubmodulesDiscardOnlyStaleRemoteCache(t *testing.T) {
	for _, kind := range []string{"stale", "clone-cache", "live", "head", "branch", "head-reflog", "oldest-reflog", "branch-reflog", "orig-head", "loose-blob", "other-remote", "custom-fetch", "manual-cache-ref"} {
		t.Run(kind, func(t *testing.T) {
			f, wt, child := disposableSubmoduleFixture(t)
			initial := f.run(child, "rev-parse", "HEAD")
			writer := filepath.Join(f.root, "writer")
			f.run(f.root, "clone", f.bare, writer)
			f.run(writer, "commit", "--allow-empty", "-m", "generated upstream commit")
			extra := f.run(writer, "rev-parse", "HEAD")
			f.run(writer, "push", "origin", "HEAD:refs/heads/generated")
			f.run(child, "fetch", "origin")
			if kind != "live" {
				f.run(writer, "push", "origin", ":refs/heads/generated")
			}
			switch kind {
			case "clone-cache":
				admin := f.run(child, "rev-parse", "--absolute-git-dir")
				if err := os.Remove(filepath.Join(admin, "logs", "refs", "remotes", "origin", "generated")); err != nil {
					t.Fatal(err)
				}
			case "head":
				f.run(child, "checkout", "--detach", extra)
				f.run(wt.Path, "commit", "-am", "record child")
				wt.OID = f.run(wt.Path, "rev-parse", "HEAD")
			case "branch":
				f.run(child, "branch", "-f", "main", extra)
			case "head-reflog", "oldest-reflog":
				f.run(child, "checkout", "--detach", extra)
				f.run(child, "checkout", "--detach", initial)
				if kind == "oldest-reflog" {
					admin := f.run(child, "rev-parse", "--absolute-git-dir")
					log := filepath.Join(admin, "logs", "HEAD")
					data, err := os.ReadFile(log)
					if err != nil {
						t.Fatal(err)
					}
					_, last, ok := strings.CutLast(strings.TrimSuffix(string(data), "\n"), "\n")
					if !ok || !strings.HasPrefix(last, extra+" "+initial+" ") {
						t.Fatal("fixture lacks old-only recovery endpoint")
					}
					f.write(log, last+"\n")
				}
			case "branch-reflog":
				f.run(child, "branch", "-f", "main", extra)
				f.run(child, "branch", "-f", "main", initial)
			case "orig-head":
				f.run(child, "update-ref", "ORIG_HEAD", extra)
			case "loose-blob":
				path := filepath.Join(f.root, "unpublished")
				f.write(path, "unpublished loose data\n")
				f.run(child, "hash-object", "-w", path)
			case "other-remote":
				f.run(child, "update-ref", "refs/remotes/other/generated", extra)
			case "custom-fetch":
				f.run(child, "config", "remote.origin.fetch", "+refs/tags/*:refs/remotes/origin/*")
			case "manual-cache-ref":
				tree := f.run(child, "rev-parse", "HEAD^{tree}")
				local := f.run(child, "commit-tree", tree, "-p", extra, "-m", "local recovery")
				f.run(child, "update-ref", "refs/remotes/origin/generated", local)
			}
			refs := f.run(child, "show-ref")
			err := f.repo.checkDisposableSubmodules(t.Context(), wt)
			wantDisposable := kind == "stale" || kind == "clone-cache" || kind == "live"
			if (err == nil) != wantDisposable {
				t.Fatalf("%s disposable=%v, want %v: %v", kind, err == nil, wantDisposable, err)
			}
			if got := f.run(child, "show-ref"); got != refs {
				t.Fatal("read-only proof changed refs")
			}
			if kind == "stale" {
				f.pullRequests("MERGED", map[string]string{wt.Branch: wt.OID})
				result := cleanFixture(t, f, false)
				if result.WorktreesRemoved != 1 {
					t.Fatalf("stale cache still blocked completed-worktree cleanup: %+v", result)
				}
				if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
					t.Fatalf("completed worktree remains: %v", err)
				}
			}
		})
	}
}

func TestDisposableSubmodulesNestedAndUninitialized(t *testing.T) {
	f, wt, child := disposableSubmoduleFixture(t)
	f.run(child, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "grand")
	f.run(child, "commit", "-am", "record nested child")
	f.run(child, "push", "origin", "HEAD:refs/heads/nested")
	f.run(wt.Path, "add", "child")
	f.run(wt.Path, "commit", "-m", "update child")
	wt.OID = f.run(wt.Path, "rev-parse", "HEAD")
	plan, err := f.repo.disposableSubmodulePlan(t.Context(), wt)
	if err != nil || len(plan.modules) != 2 {
		t.Fatalf("recoverable nested child rejected: %+v, %v", plan, err)
	}
	uninitialized := filepath.Join(f.root, "uninitialized")
	oid := f.branch("uninitialized", uninitialized)
	plan, err = f.repo.disposableSubmodulePlan(t.Context(), Worktree{Path: uninitialized, Branch: "uninitialized", OID: oid})
	if err != nil || len(plan.modules) != 0 || !plan.hasSubmodules {
		t.Fatalf("uninitialized gitlinks without storage rejected: %+v, %v", plan, err)
	}
}
