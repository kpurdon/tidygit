package main

import (
	"os"
	"path/filepath"
	"runtime"
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
			if result.WorktreesRemoved != 0 || result.BranchesDeleted != 0 || len(result.Archives) != 0 {
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
	if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 || len(result.Archives) != 0 {
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
			result := cleanFixtureWithArchives(t, f, mode == "dry-run")
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
