package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupKeepsDanglingSubmodulePointer(t *testing.T) {
	for _, mode := range []string{"relative", "absolute", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f, opts := repairFixture(t, "codex", repairedReport, "")
			opts.Fix = mode == "dry-run"
			opts.DryRun = mode == "dry-run"
			f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
			f.run(f.dir, "commit", "-am", "record child")
			path := filepath.Join(f.root, "completed")
			oid := f.branch("completed", path)
			f.run(path, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
			pointer := filepath.Join(path, "child", ".git")
			target := "../../missing-metadata"
			if mode == "absolute" {
				target = filepath.Join(f.root, "missing-metadata")
			}
			body := "gitdir: " + target + "\n"
			f.write(pointer, body)
			precious := filepath.Join(path, "child", "tracked")
			f.write(precious, "local submodule work\n")
			f.branch("other-completed", "")
			f.pullRequests("MERGED", map[string]string{"completed": oid, "other-completed": oid})
			result := cleanFixtureOptions(t, f, opts)
			if result.WorktreesRemoved != 0 || result.Repaired || repairCallCount(t, "TEST_REPAIR_CALLS") != 0 {
				t.Fatalf("protected checkout was removed or repaired: %+v", result)
			}
			f.assertBranch("completed", true)
			f.assertBranch("other-completed", opts.DryRun)
			for file, want := range map[string]string{pointer: body, precious: "local submodule work\n"} {
				if got, err := os.ReadFile(file); err != nil || string(got) != want {
					t.Fatalf("protected file changed: %s: %q, %v", file, got, err)
				}
			}
		})
	}
}

func TestCleanupKeepsSubmoduleReplacedWithFile(t *testing.T) {
	f := newCleanupFixture(t)
	f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
	f.run(f.dir, "commit", "-am", "record child")
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	child := filepath.Join(path, "child")
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	f.write(child, "local replacement\n")
	f.pullRequests("MERGED", map[string]string{"completed": oid})
	cleanFixture(t, f, false)
	f.assertBranch("completed", true)
	if got, err := os.ReadFile(child); err != nil || string(got) != "local replacement\n" {
		t.Fatalf("replacement file changed: %q, %v", got, err)
	}
}

func TestCompletedWorktreeKeepsSymlinkedSubmodulesWithoutError(t *testing.T) {
	for _, kind := range []string{"shared child", "dangling child", "shared ancestor", "dangling ancestor"} {
		t.Run(kind, func(t *testing.T) {
			f := newCleanupFixture(t)
			f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "modules/child")
			f.run(f.dir, "commit", "-am", "record child")
			shared := filepath.Join(f.dir, "modules", "child")
			sharedHead := f.run(shared, "rev-parse", "HEAD")
			path := filepath.Join(f.root, "completed")
			oid := f.branch("completed", path)
			link := filepath.Join(path, "modules", "child")
			target := shared
			if strings.Contains(kind, "ancestor") {
				link = filepath.Dir(link)
				target = filepath.Dir(shared)
			}
			if strings.Contains(kind, "dangling") {
				target = filepath.Join(f.root, "missing")
			}
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			f.pullRequests("MERGED", map[string]string{"completed": oid})
			result := cleanFixture(t, f, false)
			if result.WorktreesRemoved != 0 || result.BranchesDeleted != 0 {
				t.Fatalf("symlinked checkout was removed: %+v", result)
			}
			f.assertBranch("completed", true)
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Fatalf("checkout link changed: %q %v", got, err)
			}
			if got := f.run(shared, "rev-parse", "HEAD"); got != sharedHead {
				t.Fatalf("shared submodule changed: %s", got)
			}
		})
	}
}

func TestDefaultBranchUpdateSynchronizesSubmodules(t *testing.T) {
	for _, mode := range []string{"fast-forward", "rebase", "ignored collision", "dirty", "pin mismatch", "uninitialized", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f := newCleanupFixture(t)
			f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
			child := filepath.Join(f.dir, "child")
			oldChild := f.run(child, "rev-parse", "HEAD")
			f.run(f.dir, "commit", "-am", "add child")
			oldParent := f.run(f.dir, "rev-parse", "HEAD")
			f.write(filepath.Join(child, "tracked"), "new upstream content\n")
			if mode == "ignored collision" {
				f.write(filepath.Join(child, "ignored"), "upstream file\n")
				f.run(child, "add", "--force", "ignored")
			}
			f.run(child, "commit", "-am", "update child")
			newChild := f.run(child, "rev-parse", "HEAD")
			f.run(f.dir, "commit", "-am", "update child pin")
			f.run(f.dir, "push")
			f.run(f.dir, "reset", "--hard", oldParent)
			f.run(child, "checkout", "--detach", oldChild)
			f.run(f.dir, "config", "submodule.recurse", "true")
			switch mode {
			case "ignored collision":
				f.write(filepath.Join(child, "ignored"), "precious local bytes\n")
			case "dirty":
				f.write(filepath.Join(child, "tracked"), "precious local bytes\n")
			case "pin mismatch":
				f.run(child, "checkout", "--detach", newChild)
			case "uninitialized":
				f.run(f.dir, "submodule", "deinit", "child")
			}
			opts := defaultOptions()
			opts.Auto = true
			opts.Rebase = mode == "rebase"
			opts.DryRun = mode == "dry-run"
			result, err := clean(t.Context(), f.repo, opts)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ignored collision" {
				if len(result.Errors) == 0 {
					t.Fatal("update must report the ignored-file collision")
				}
				body, err := os.ReadFile(filepath.Join(child, "ignored"))
				if err != nil || string(body) != "precious local bytes\n" {
					t.Fatalf("ignored content lost: %q %v", body, err)
				}
				return
			}
			if len(result.Errors) != 0 {
				t.Fatalf("cleanup failed: %+v", result)
			}
			if mode == "dirty" || mode == "pin mismatch" || mode == "dry-run" {
				if got := f.run(f.dir, "rev-parse", "HEAD"); got != oldParent {
					t.Fatalf("protected checkout advanced: %s", got)
				}
				if mode == "dirty" {
					body, err := os.ReadFile(filepath.Join(child, "tracked"))
					if err != nil || string(body) != "precious local bytes\n" {
						t.Fatalf("local changes lost: %q %v", body, err)
					}
				}
				return
			}
			if mode == "uninitialized" {
				if _, err := os.Stat(filepath.Join(child, ".git")); !os.IsNotExist(err) {
					t.Fatalf("uninitialized child was changed: %v", err)
				}
			} else if got := f.run(child, "rev-parse", "HEAD"); got != newChild {
				t.Fatalf("submodule remained at %s; parent requires %s", got, newChild)
			}
			if status := f.run(f.dir, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
				t.Fatalf("update left dirty checkout: %s", status)
			}
		})
	}
}

func TestDefaultBranchUpdateSynchronizesNestedSubmodules(t *testing.T) {
	f := newCleanupFixture(t)
	f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
	child := filepath.Join(f.dir, "child")
	f.run(child, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "nested")
	nested := filepath.Join(child, "nested")
	oldNested := f.run(nested, "rev-parse", "HEAD")
	f.run(child, "commit", "-am", "add nested child")
	oldChild := f.run(child, "rev-parse", "HEAD")
	f.run(f.dir, "commit", "-am", "add child")
	oldParent := f.run(f.dir, "rev-parse", "HEAD")
	f.write(filepath.Join(nested, "tracked"), "new nested content\n")
	f.run(nested, "commit", "-am", "update nested child")
	newNested := f.run(nested, "rev-parse", "HEAD")
	f.run(child, "commit", "-am", "update nested pin")
	newChild := f.run(child, "rev-parse", "HEAD")
	f.run(f.dir, "commit", "-am", "update child pin")
	f.run(f.dir, "push")
	f.run(f.dir, "reset", "--hard", oldParent)
	f.run(child, "checkout", "--detach", oldChild)
	f.run(nested, "checkout", "--detach", oldNested)
	f.write(filepath.Join(nested, "ignored"), "local cache\n")
	cleanFixture(t, f, false)
	if got := f.run(child, "rev-parse", "HEAD"); got != newChild {
		t.Fatalf("child remained at %s; want %s", got, newChild)
	}
	if got := f.run(nested, "rev-parse", "HEAD"); got != newNested {
		t.Fatalf("nested child remained at %s; want %s", got, newNested)
	}
	if body, err := os.ReadFile(filepath.Join(nested, "ignored")); err != nil || string(body) != "local cache\n" {
		t.Fatalf("nested ignored file changed: %q %v", body, err)
	}
	if status := f.run(f.dir, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("update left nested submodule drift: %s", status)
	}
}

func TestDefaultBranchUpdateFetchesMissingSubmoduleCommit(t *testing.T) {
	f := newCleanupFixture(t)
	f.run(f.dir, "-c", "protocol.file.allow=always", "submodule", "add", f.bare, "child")
	child := filepath.Join(f.dir, "child")
	f.run(f.dir, "commit", "-am", "add child")
	oldParent := f.run(f.dir, "rev-parse", "HEAD")
	writer := filepath.Join(f.root, "writer")
	f.run(f.root, "clone", f.bare, writer)
	f.write(filepath.Join(writer, "tracked"), "new remote child content\n")
	f.run(writer, "commit", "-am", "update remote child")
	newChild := f.run(writer, "rev-parse", "HEAD")
	f.run(writer, "push", "origin", "HEAD:refs/heads/interfaces")
	f.run(f.dir, "update-index", "--cacheinfo", "160000,"+newChild+",child")
	f.run(f.dir, "commit", "-m", "update child pin")
	f.run(f.dir, "push")
	f.run(f.dir, "reset", "--hard", oldParent)
	f.run(f.dir, "config", "fetch.recurseSubmodules", "false")
	f.run(child, "config", "protocol.file.allow", "always")
	cleanFixture(t, f, false)
	if got := f.run(child, "rev-parse", "HEAD"); got != newChild {
		t.Fatalf("missing commit not fetched and checked out: %s", got)
	}
}

func TestCompletedWorktreeKeepsNestedBareRepository(t *testing.T) {
	for _, mode := range []string{"backup.git", "backup", "missing HEAD", "objects and refs only"} {
		t.Run(mode, func(t *testing.T) {
			f := newCleanupFixture(t)
			f.write(filepath.Join(f.dir, ".gitignore"), "backup*/\n")
			f.run(f.dir, "commit", "-am", "ignore backup directory")
			f.run(f.dir, "push")
			path := filepath.Join(f.root, "completed")
			oid := f.branch("completed", path)
			name := mode
			if mode == "missing HEAD" || mode == "objects and refs only" {
				name = "backup"
			}
			bare := filepath.Join(path, name)
			f.run(path, "clone", "--bare", "--no-local", f.dir, bare)
			tree := f.run(f.dir, "rev-parse", "HEAD^{tree}")
			unique := f.run(path, "--git-dir", bare, "commit-tree", tree, "-p", oid, "-m", "unique recovery history")
			f.run(path, "--git-dir", bare, "update-ref", "refs/heads/recovery", unique)
			if mode == "missing HEAD" || mode == "objects and refs only" {
				if err := os.Remove(filepath.Join(bare, "HEAD")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "objects and refs only" {
				for _, name := range []string{"config", "packed-refs"} {
					if err := os.Remove(filepath.Join(bare, name)); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
			}
			f.pullRequests("MERGED", map[string]string{"completed": oid})
			result := cleanFixture(t, f, false)
			if result.WorktreesRemoved != 0 || result.BranchesDeleted != 0 {
				t.Fatalf("nested bare recovery history was discarded: %+v", result)
			}
			f.assertBranch("completed", true)
			if mode == "missing HEAD" || mode == "objects and refs only" {
				f.write(filepath.Join(bare, "HEAD"), "ref: refs/heads/main\n")
			}
			if got := f.run(path, "--git-dir", bare, "rev-parse", "recovery"); got != unique {
				t.Fatalf("nested recovery ref changed: %s", got)
			}
			if got := f.run(path, "--git-dir", bare, "cat-file", "-t", unique); got != "commit" {
				t.Fatalf("nested recovery commit missing: %s", got)
			}
		})
	}
}

func TestCompletedWorktreeDiscardsOrdinaryIgnoredDirectory(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	ignored := filepath.Join(path, "ignored")
	if err := os.Mkdir(ignored, 0o700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(ignored, "HEAD"), "ordinary application data\n")
	f.write(filepath.Join(ignored, "cache"), "disposable bytes\n")
	f.pullRequests("MERGED", map[string]string{"completed": oid})
	result := cleanFixture(t, f, false)
	if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 {
		t.Fatalf("ordinary ignored directory prevented cleanup: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("completed checkout remains: %v", err)
	}
}

func TestCompletedWorktreeDiscardsIgnoredPackageWithGitNames(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	ignored := filepath.Join(path, "ignored")
	for _, dir := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(ignored, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.write(filepath.Join(ignored, "objects", "blob.py"), "class Blob: pass\n")
	f.write(filepath.Join(ignored, "refs", "head.py"), "class Head: pass\n")
	f.pullRequests("MERGED", map[string]string{"completed": oid})
	result := cleanFixture(t, f, false)
	if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 {
		t.Fatalf("ordinary package names blocked cleanup: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("completed checkout remains: %v", err)
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
			supported, err := f.repo.submoduleStoragePolicy(t.Context(), Worktree{Path: path, Branch: "completed", OID: oid})
			if err == nil || !supported {
				t.Fatalf("unsupported storage not identified: %v %v", supported, err)
			}
			if _, err := os.Stat(filepath.Join(child, "tracked")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletedSubmoduleCleanupDiscardsIgnoredFilesAndKeepsDirtyChildren(t *testing.T) {
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
				if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 {
					t.Fatalf("clean submodule cleanup failed: %+v", result)
				}
			} else {
				if result.WorktreesRemoved != 0 || result.BranchesDeleted != 0 {
					t.Fatalf("protected submodule changed: %+v", result)
				}
				if _, err := os.Stat(filepath.Join(child, "ignored")); err != nil {
					t.Fatal(err)
				}
				f.assertBranch("completed", true)
				if mode == "dry-run" && result.WorktreesPlanned != 1 {
					t.Fatalf("submodule removal not previewed: %+v", result)
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
	supported, err := f.repo.submoduleStoragePolicy(t.Context(), Worktree{Path: path, Branch: "completed", OID: oid})
	if err == nil || !supported {
		t.Fatalf("deinitialized external object storage accepted: %v %v", supported, err)
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
	hasModules, err := f.repo.submoduleStoragePolicy(t.Context(), Worktree{Path: path, Branch: "completed", OID: oid})
	if err == nil || !hasModules {
		t.Fatalf("external checkout metadata accepted: %v %v (Git directory %s)", hasModules, err, admin)
	}
}
