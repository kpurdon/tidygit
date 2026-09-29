package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type cleanupFixture struct {
	t                         *testing.T
	root, dir, bare, git, prs string
	repo                      repository
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture executables use POSIX shell")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f := &cleanupFixture{t: t, root: t.TempDir(), git: git}
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	f.dir = filepath.Join(f.root, "repo")
	f.bare = filepath.Join(f.root, "origin.git")
	f.prs = filepath.Join(f.root, "prs.json")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	f.run(f.root, "init", "--bare", "--initial-branch=main", f.bare)
	f.run(f.root, "init", "--initial-branch=main", f.dir)
	f.write(filepath.Join(f.dir, "tracked"), "base\n")
	f.write(filepath.Join(f.dir, ".gitignore"), "ignored\n")
	f.run(f.dir, "add", ".")
	f.run(f.dir, "commit", "-m", "initial")
	f.run(f.dir, "remote", "add", "origin", "https://github.com/audit/repo.git")
	f.run(f.dir, "config", "url."+f.bare+".insteadOf", "https://github.com/audit/repo.git")
	f.run(f.dir, "push", "-u", "origin", "main")
	bin := filepath.Join(f.root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	// Preserve the configured identity while routing network operations to the bare repository.
	f.write(filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = remote ] && [ \"$2\" = get-url ]; then exec \"$TEST_REAL_GIT\" config --get \"remote.$3.url\"; fi\nexec \"$TEST_REAL_GIT\" \"$@\"\n")
	f.write(filepath.Join(bin, "gh"), "#!/bin/sh\nif [ \"$1\" = api ]; then printf '%s\\n' '{\"data\":{\"repository\":{\"object\":null,\"pullRequest\":null}}}'; else cat \"$TEST_PRS\"; fi\n")
	for _, name := range []string{"git", "gh"} {
		if err := os.Chmod(filepath.Join(bin, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEST_REAL_GIT", git)
	t.Setenv("TEST_PRS", f.prs)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.write(f.prs, "[]")
	f.repo, err = discoverRepo(t.Context(), f.dir, defaultOptions().Timeout)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *cleanupFixture) run(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(f.git, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSuffix(string(out), "\n")
}
func (f *cleanupFixture) write(path, body string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		f.t.Fatal(err)
	}
}
func (f *cleanupFixture) branch(name, path string) string {
	f.t.Helper()
	if path == "" {
		f.run(f.dir, "branch", name)
	} else {
		f.run(f.dir, "worktree", "add", "-b", name, path)
	}
	return f.run(f.dir, "rev-parse", name)
}
func (f *cleanupFixture) pullRequests(state string, branches map[string]string) {
	f.t.Helper()
	var prs []map[string]any
	for name, oid := range branches {
		prs = append(prs, map[string]any{"number": 1, "headRefName": name, "headRefOid": oid, "state": state, "headRepository": map[string]string{"name": "repo"}, "headRepositoryOwner": map[string]string{"login": "audit"}})
	}
	body, err := json.Marshal(prs)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(f.prs, string(body))
}
func (f *cleanupFixture) assertBranch(name string, want bool) {
	f.t.Helper()
	cmd := exec.Command(f.git, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Dir = f.dir
	if got := cmd.Run() == nil; got != want {
		f.t.Fatalf("branch %q exists=%v, want %v", name, got, want)
	}
}
func checkCounts(t *testing.T, r repoResult, want int) {
	t.Helper()
	if r.BranchesTotal != want || r.BranchesSkipped+r.BranchesDeleted+r.BranchesPlanned != want {
		t.Fatalf("branch accounting: %+v, original candidates=%d", r, want)
	}
}
func cleanFixture(t *testing.T, f *cleanupFixture, dry bool) repoResult {
	t.Helper()
	opts := defaultOptions()
	opts.Auto = true
	opts.DryRun = dry
	r, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(r.Errors) > 0 {
		t.Fatalf("clean: %v, %+v", err, r)
	}
	return r
}

func TestAutoPreservesDirtyAndNewerWork(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			f := newCleanupFixture(t)
			prs := map[string]string{}
			for _, kind := range []string{"tracked", "untracked", "ignored", "clean", "newer", "locked"} {
				path := filepath.Join(f.root, kind)
				prs[kind] = f.branch(kind, path)
				switch kind {
				case "tracked", "untracked", "ignored":
					f.write(filepath.Join(path, kind), "keep me\n")
				case "newer":
					f.write(filepath.Join(path, "tracked"), "new commit\n")
					f.run(path, "commit", "-am", "new local commit")
				case "locked":
					f.run(f.dir, "worktree", "lock", path)
				}
			}
			f.pullRequests(state, prs)
			r := cleanFixture(t, f, false)
			checkCounts(t, r, 6)
			if r.BranchesDeleted != 2 || r.WorktreesRemoved != 2 {
				t.Fatalf("unexpected deletions: %+v", r)
			}
			for name := range prs {
				f.assertBranch(name, name != "clean" && name != "ignored")
				if name != "clean" && name != "ignored" {
					if _, err := os.Stat(filepath.Join(f.root, name)); err != nil {
						t.Fatal(err)
					}
				}
			}

			if len(r.Archives) != 1 {
				t.Fatalf("missing ignored-file archive: %+v", r)
			}
			archived, err := os.ReadFile(filepath.Join(r.Archives[0], "files", "ignored"))
			if err != nil || string(archived) != "keep me\n" {
				t.Fatalf("ignored data lost: %q %v", archived, err)
			}
			for _, kind := range []string{"tracked", "untracked"} {
				body, err := os.ReadFile(filepath.Join(f.root, kind, kind))
				if err != nil || string(body) != "keep me\n" {
					t.Fatalf("lost %s data: %q %v", kind, body, err)
				}
			}
		})
	}
}

func TestCurrentAndMainWorktreesProtected(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "current")
	oid := f.branch("current", path)
	f.run(f.dir, "switch", "-c", "primary")
	f.run(f.dir, "worktree", "add", filepath.Join(f.root, "default"), "main")
	f.pullRequests("MERGED", map[string]string{"current": oid, "primary": oid, "main": oid})
	var err error
	f.repo, err = discoverRepo(t.Context(), path, defaultOptions().Timeout)
	if err != nil {
		t.Fatal(err)
	}
	r := cleanFixture(t, f, false)
	checkCounts(t, r, 2)
	if r.WorktreesRemoved != 0 || r.BranchesDeleted != 0 {
		t.Fatalf("protected work removed: %+v", r)
	}
	for _, name := range []string{"current", "primary", "main"} {
		f.assertBranch(name, true)
	}
}

func TestDryRunDoesNotMutateRepository(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "merged")
	oid := f.branch("merged", path)
	f.pullRequests("MERGED", map[string]string{"merged": oid})
	f.write(filepath.Join(path, "ignored"), "do not move\n")
	f.write(filepath.Join(f.dir, "tracked"), "staged\n")
	f.run(f.dir, "add", "tracked")
	snapshot := func() map[string]string {
		files := map[string]string{}
		err := filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				files[path] = string(body)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := snapshot()
	r := cleanFixture(t, f, true)
	after := snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("dry run changed repository files, index, or refs")
	}
	checkCounts(t, r, 1)
	if r.BranchesPlanned != 1 || r.WorktreesPlanned != 1 {
		t.Fatalf("missing plan: %+v", r)
	}

	if _, err := os.Stat(filepath.Join(f.root, "state")); !os.IsNotExist(err) {
		t.Fatalf("dry run created state: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "ignored")); err != nil || string(data) != "do not move\n" {
		t.Fatalf("dry run changed ignored files: %q %v", data, err)
	}
	f.assertBranch("merged", true)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupFailureStopsDeletionAndFailsAll(t *testing.T) {
	for _, kind := range []string{"fetch", "ff", "rebase", "in-progress", "github"} {
		t.Run(kind, func(t *testing.T) {
			f := newCleanupFixture(t)
			oid := f.branch("merged", "")
			f.pullRequests("MERGED", map[string]string{"merged": oid})
			opts := defaultOptions()
			opts.Auto = true
			switch kind {
			case "fetch":
				f.run(f.dir, "remote", "add", "broken", filepath.Join(f.root, "missing"))
			case "ff", "rebase":
				f.write(filepath.Join(f.dir, "tracked"), "remote\n")
				f.run(f.dir, "commit", "-am", "remote change")
				f.run(f.dir, "push")
				f.run(f.dir, "reset", "--hard", "HEAD^")
				f.write(filepath.Join(f.dir, "tracked"), "local\n")
				f.run(f.dir, "commit", "-am", "local change")
				opts.Rebase = kind == "rebase"
			case "in-progress":
				f.write(filepath.Join(f.dir, ".git", "MERGE_HEAD"), oid+"\n")
			case "github":
				f.write(f.prs, "invalid JSON")
			}
			if err := cleanAll(t.Context(), f.root, opts); err == nil {
				t.Fatal("cleanup failure returned success")
			}
			f.assertBranch("merged", true)
		})
	}
}

func TestAllPreservesCallerLinkedWorktree(t *testing.T) {
	f := newCleanupFixture(t)
	caller := filepath.Join(f.root, "caller")
	oid := f.branch("caller", caller)
	f.pullRequests("MERGED", map[string]string{"caller": oid})
	t.Chdir(caller)
	opts := defaultOptions()
	opts.Auto = true
	if err := cleanAll(t.Context(), f.root, opts); err != nil {
		t.Fatal(err)
	}
	f.assertBranch("caller", true)
	if _, err := os.Stat(filepath.Join(caller, "tracked")); err != nil {
		t.Fatalf("all removed its caller worktree: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil || canonicalPath(cwd) != canonicalPath(caller) {
		t.Fatalf("caller directory no longer usable: %q %v", cwd, err)
	}
}

func TestCleanWorktreeWithOperationInProgressIsPreserved(t *testing.T) {
	for _, dry := range []bool{false, true} {
		name := "auto"
		if dry {
			name = "dry-run"
		}
		t.Run(name, func(t *testing.T) {
			f := newCleanupFixture(t)
			path := filepath.Join(f.root, "cherry-picking")
			oid := f.branch("cherry-picking", path)
			f.pullRequests("MERGED", map[string]string{"cherry-picking": oid})
			state := f.run(path, "rev-parse", "--path-format=absolute", "--git-path", "CHERRY_PICK_HEAD")
			f.write(state, oid+"\n")
			if status := f.run(path, "status", "--porcelain"); status != "" {
				t.Fatalf("fixture should have clean files: %q", status)
			}
			r := cleanFixture(t, f, dry)
			checkCounts(t, r, 1)
			if r.WorktreesRemoved != 0 || r.WorktreesPlanned != 0 || r.BranchesDeleted != 0 || r.BranchesPlanned != 0 {
				t.Fatalf("in-progress worktree selected for removal: %+v", r)
			}
			f.assertBranch("cherry-picking", true)
			if body, err := os.ReadFile(state); err != nil || string(body) != oid+"\n" {
				t.Fatalf("operation state lost: %q %v", body, err)
			}
		})
	}
}

func TestRemoteUpdatePreservesIgnoredLocalFile(t *testing.T) {
	for _, rebase := range []bool{false, true} {
		name := "fast-forward"
		if rebase {
			name = "rebase"
		}
		t.Run(name, func(t *testing.T) {
			f := newCleanupFixture(t)
			original := f.run(f.dir, "rev-parse", "main")
			f.write(filepath.Join(f.dir, "ignored"), "remote contents\n")
			f.run(f.dir, "add", "--force", "ignored")
			f.run(f.dir, "commit", "-m", "start tracking previously ignored file")
			f.run(f.dir, "push")
			f.run(f.dir, "reset", "--hard", original)
			f.write(filepath.Join(f.dir, "ignored"), "precious local contents\n")
			opts := defaultOptions()
			opts.Auto = true
			opts.Rebase = rebase
			r, err := clean(t.Context(), f.repo, opts)
			if err != nil {
				t.Fatal(err)
			}
			if rebase && len(r.Errors) != 0 {
				t.Fatalf("rebase should skip update: %+v", r)
			}
			if !rebase && len(r.Errors) == 0 {
				t.Fatal("fast-forward should refuse overwriting ignored file")
			}
			body, err := os.ReadFile(filepath.Join(f.dir, "ignored"))
			if err != nil || string(body) != "precious local contents\n" {
				t.Fatalf("ignored file overwritten: %q %v", body, err)
			}
			if got := f.run(f.dir, "rev-parse", "main"); got != original {
				t.Fatalf("default branch advanced despite protected ignored file: %s", got)
			}
		})
	}
}

func TestClosedPRCleanup(t *testing.T) {
	for _, dry := range []bool{false, true} {
		name := "auto"
		if dry {
			name = "dry-run"
		}
		t.Run(name, func(t *testing.T) {
			f := newCleanupFixture(t)
			path := filepath.Join(f.root, "closed-worktree")
			f.branch("closed-worktree", path)
			f.write(filepath.Join(path, "tracked"), "closed without merging\n")
			f.run(path, "commit", "-am", "abandoned work")
			oid := f.run(path, "rev-parse", "HEAD")
			f.run(f.dir, "branch", "closed-branch", oid)
			f.pullRequests("CLOSED", map[string]string{"closed-worktree": oid, "closed-branch": oid})
			r := cleanFixture(t, f, dry)
			checkCounts(t, r, 2)
			if dry {
				if r.BranchesPlanned != 2 || r.WorktreesPlanned != 1 || r.BranchesDeleted != 0 || r.WorktreesRemoved != 0 {
					t.Fatalf("unexpected preview: %+v", r)
				}
			} else if r.BranchesDeleted != 2 || r.WorktreesRemoved != 1 {
				t.Fatalf("closed work retained: %+v", r)
			}
			f.assertBranch("closed-worktree", dry)
			f.assertBranch("closed-branch", dry)
			_, err := os.Stat(path)
			if dry && err != nil {
				t.Fatal(err)
			}
			if !dry && !os.IsNotExist(err) {
				t.Fatalf("worktree still exists: %v", err)
			}
		})
	}
}

func TestArchiveFailureKeepsWorktree(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "closed")
	oid := f.branch("closed", path)
	f.pullRequests("CLOSED", map[string]string{"closed": oid})
	f.write(filepath.Join(path, "ignored"), "precious\n")
	if err := os.MkdirAll(filepath.Join(f.root, "state", "tidygit"), 0700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "state", "tidygit", "archives"), "blocks archive directory")
	opts := defaultOptions()
	opts.Auto = true
	r, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(r.Errors) == 0 || r.WorktreesRemoved != 0 || r.BranchesDeleted != 0 {
		t.Fatalf("archive failure not retained: %+v, %v", r, err)
	}
	f.assertBranch("closed", true)
	if data, err := os.ReadFile(filepath.Join(path, "ignored")); err != nil || string(data) != "precious\n" {
		t.Fatalf("original data lost: %q %v", data, err)
	}
}

func TestLocalMarkerSkipsNetworkUntilForceOrLocalChange(t *testing.T) {
	f := newCleanupFixture(t)
	log := filepath.Join(f.root, "fetches")
	t.Setenv("TEST_FETCH_LOG", log)
	f.write(filepath.Join(f.root, "bin", "git"), "#!/bin/sh\nif [ \"$1\" = remote ] && [ \"$2\" = get-url ]; then exec \"$TEST_REAL_GIT\" config --get \"remote.$3.url\"; fi\nif [ \"$1\" = fetch ]; then echo fetch >> \"$TEST_FETCH_LOG\"; fi\nexec \"$TEST_REAL_GIT\" \"$@\"\n")
	first := cleanFixture(t, f, false)
	if first.Unchanged {
		t.Fatal("first run skipped")
	}
	second := cleanFixture(t, f, false)
	if !second.Unchanged {
		t.Fatal("unchanged repository was processed again")
	}
	data, err := os.ReadFile(log)
	if err != nil || strings.Count(string(data), "fetch") != 1 {
		t.Fatalf("unchanged run fetched: %q %v", data, err)
	}
	opts := defaultOptions()
	opts.Auto = true
	opts.Force = true
	forced, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(forced.Errors) != 0 || forced.Unchanged {
		t.Fatalf("force did not process: %+v %v", forced, err)
	}
	f.branch("new-worktree", filepath.Join(f.root, "new-worktree"))
	changed := cleanFixture(t, f, false)
	if changed.Unchanged {
		t.Fatal("new worktree did not invalidate marker")
	}
	data, err = os.ReadFile(log)
	if err != nil || strings.Count(string(data), "fetch") != 3 {
		t.Fatalf("expected initial, forced and changed fetches: %q %v", data, err)
	}
	f.write(f.prs, "invalid JSON")
	failed, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(failed.Errors) == 0 {
		t.Fatalf("expected forced failure: %+v %v", failed, err)
	}
	f.write(f.prs, "[]")
	retried := cleanFixture(t, f, false)
	if retried.Unchanged {
		t.Fatal("failed forced run left a matching marker")
	}

}

func TestLocalMarkerIncludesOwnUpdateAndCleanup(t *testing.T) {
	f := newCleanupFixture(t)
	old := f.run(f.dir, "rev-parse", "HEAD")
	path := filepath.Join(f.root, "completed")
	oid := f.branch("completed", path)
	f.run(f.dir, "config", "branch.completed.remote", "origin")
	f.run(f.dir, "config", "branch.completed.merge", "refs/heads/completed")
	f.pullRequests("CLOSED", map[string]string{"completed": oid})
	f.write(filepath.Join(f.dir, "tracked"), "remote update\n")
	f.run(f.dir, "commit", "-am", "remote update")
	updated := f.run(f.dir, "rev-parse", "HEAD")
	f.run(f.dir, "push", "origin", "main")
	f.run(f.dir, "reset", "--hard", old)
	first := cleanFixture(t, f, false)
	if first.WorktreesRemoved != 1 || first.BranchesDeleted != 1 || f.run(f.dir, "rev-parse", "HEAD") != updated {
		t.Fatalf("fixture did not update and clean: %+v", first)
	}
	// Any GitHub lookup would fail; the completed state must hit immediately.
	f.write(f.prs, "invalid JSON")
	second := cleanFixture(t, f, false)
	if !second.Unchanged {
		t.Fatal("own update/removal forced an unnecessary full pass")
	}
}

func TestLocalMarkerDoesNotStampConcurrentWork(t *testing.T) {
	for _, mutation := range []string{"branch", "worktree", "file"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCleanupFixture(t)
			retained := filepath.Join(f.root, "retained")
			oid := f.branch("retained", retained)
			f.pullRequests("OPEN", map[string]string{"retained": oid})
			t.Setenv("TEST_MUTATION", mutation)
			t.Setenv("TEST_REPO", f.dir)
			t.Setenv("TEST_MUTATED", filepath.Join(f.root, "mutated"))
			t.Setenv("TEST_NEW_WORKTREE", filepath.Join(f.root, "unseen"))
			t.Setenv("TEST_EDIT_PATH", filepath.Join(retained, "ignored"))
			f.write(filepath.Join(f.root, "bin", "gh"), `#!/bin/sh
if [ ! -e "$TEST_MUTATED" ]; then
  touch "$TEST_MUTATED"
  case "$TEST_MUTATION" in
    branch) "$TEST_REAL_GIT" -C "$TEST_REPO" branch unseen ;;
    worktree) "$TEST_REAL_GIT" -C "$TEST_REPO" worktree add -b unseen "$TEST_NEW_WORKTREE" >/dev/null ;;
    file) printf 'concurrent local work\n' > "$TEST_EDIT_PATH" ;;
  esac
fi
if [ "$1" = api ]; then
  printf '%s\n' '{"data":{"repository":{"object":null,"pullRequest":null}}}'
else
  cat "$TEST_PRS"
fi
`)
			cleanFixture(t, f, false)
			marker, err := runMarkerPath(f.repo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("concurrent %s was stamped as processed: %v", mutation, err)
			}
			if next := cleanFixture(t, f, false); next.Unchanged {
				t.Fatal("concurrent local work did not get a fresh pass")
			}
			if stable := cleanFixture(t, f, false); !stable.Unchanged {
				t.Fatal("stable follow-up did not hit marker")
			}
		})
	}
}

func TestUnwritableMarkerStopsForcedCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "closed")
	oid := f.branch("closed", path)
	f.pullRequests("CLOSED", map[string]string{"closed": oid})
	opts := defaultOptions()
	opts.Auto = true
	opts.Force = true
	fingerprint, err := localFingerprint(t.Context(), f.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunMarker(f.repo, fingerprint); err != nil {
		t.Fatal(err)
	}
	marker, err := runMarkerPath(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(marker)
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	r, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(r.Errors) == 0 || r.WorktreesRemoved != 0 || r.BranchesDeleted != 0 {
		t.Fatalf("cleanup ran without invalidating marker: %+v %v", r, err)
	}
	f.assertBranch("closed", true)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveNestedIgnoredDirectoriesAndPreservePrimaryEnv(t *testing.T) {
	f := newCleanupFixture(t)
	f.write(filepath.Join(f.dir, ".gitignore"), ".envrc\n**/charts/*.tgz\n")
	f.run(f.dir, "commit", "-am", "ignore environment and chart artifacts")
	f.run(f.dir, "push")
	path := filepath.Join(f.root, "merged")
	oid := f.branch("merged", path)
	f.pullRequests("MERGED", map[string]string{"merged": oid})
	f.write(filepath.Join(f.dir, ".envrc"), "primary environment\n")
	contents := map[string]string{
		".envrc":                                "linked environment\n",
		"apps/demo/chart/charts/dependency.tgz": "chart bytes\n",
		".ruff_cache/.gitignore":                "*\n",
		".ruff_cache/nested/result":             "cache bytes\n",
	}
	for name, data := range contents {
		target := filepath.Join(path, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		f.write(target, data)
	}
	r := cleanFixture(t, f, false)
	if r.WorktreesRemoved != 1 || r.BranchesDeleted != 1 || len(r.Archives) != 1 {
		t.Fatalf("ignored directory prevented cleanup: %+v", r)
	}
	for name, want := range contents {
		got, err := os.ReadFile(filepath.Join(r.Archives[0], "files", filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("archive lost %q: %q %v", name, got, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(f.dir, ".envrc"))
	if err != nil || string(got) != "primary environment\n" {
		t.Fatalf("primary environment changed: %q %v", got, err)
	}
	f.assertBranch("merged", false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
}

func TestAutoArchivesStaleDirtyWorkAndRetainsActiveProtectedWork(t *testing.T) {
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	old := time.Now().Add(-45 * 24 * time.Hour).Truncate(time.Second)
	t.Setenv("GIT_COMMITTER_DATE", old.Format(time.RFC3339))
	t.Setenv("GIT_AUTHOR_DATE", old.Format(time.RFC3339))
	oids := map[string]string{}
	for _, name := range []string{"stale-dirty", "recent", "open", "keep"} {
		path := filepath.Join(f.root, name)
		f.branch(name, path)
		f.run(path, "commit", "--allow-empty", "-m", "old local history")
		oids[name] = f.run(path, "rev-parse", "HEAD")
		f.write(filepath.Join(path, "tracked"), "staged data\n")
		f.run(path, "add", "tracked")
		f.write(filepath.Join(path, "tracked"), "unstaged data\n")
		f.write(filepath.Join(path, "untracked"), "untracked data\n")
		f.write(filepath.Join(path, "ignored"), "ignored data\n")
		if name != "recent" {
			linked := f.repo
			linked.Dir = path
			admin, err := linked.gitDir(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{path, admin} {
				if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					return os.Chtimes(path, old, old)
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	f.run(f.dir, "config", "--add", "tidygit.keep", "keep")
	f.pullRequests("OPEN", map[string]string{"open": oids["open"]})
	result := cleanFixture(t, f, false)
	if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 || len(result.Archives) != 1 {
		t.Fatalf("cleanup counts: %+v", result)
	}
	f.assertBranch("stale-dirty", false)
	if _, err := os.Stat(filepath.Join(f.root, "stale-dirty")); !os.IsNotExist(err) {
		t.Fatalf("stale worktree retained: %v", err)
	}
	archive := result.Archives[0]
	for _, name := range []string{"tracked", "untracked", "ignored"} {
		if _, err := os.Stat(filepath.Join(archive, "files", "stale-dirty", name)); err != nil {
			t.Fatalf("archived %s missing: %v", name, err)
		}
	}
	for _, name := range []string{"recent", "open", "keep"} {
		f.assertBranch(name, true)
		if got, err := os.ReadFile(filepath.Join(f.root, name, "tracked")); err != nil || string(got) != "unstaged data\n" {
			t.Fatalf("protected %s changed: %q %v", name, got, err)
		}
	}
}

func TestAutoArchivesStaleStandaloneBranchBeforeDeletingIt(t *testing.T) {
	f := newCleanupFixture(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "state"))
	old := time.Now().Add(-45 * 24 * time.Hour).Truncate(time.Second)
	t.Setenv("GIT_COMMITTER_DATE", old.Format(time.RFC3339))
	t.Setenv("GIT_AUTHOR_DATE", old.Format(time.RFC3339))
	f.run(f.dir, "switch", "-c", "old")
	f.write(filepath.Join(f.dir, "unique"), "unpublished work\n")
	f.run(f.dir, "add", "unique")
	f.run(f.dir, "commit", "-m", "old unpublished work")
	oid := f.run(f.dir, "rev-parse", "HEAD")
	f.run(f.dir, "switch", "main")
	for _, name := range []string{"refs/heads/old", "logs/refs/heads/old"} {
		if err := os.Chtimes(filepath.Join(f.repo.CommonDir, filepath.FromSlash(name)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	result := cleanFixture(t, f, false)
	if result.BranchesDeleted != 1 || len(result.Archives) != 1 {
		t.Fatalf("cleanup counts: %+v", result)
	}
	f.assertBranch("old", false)
	recovered := filepath.Join(f.root, "recovered")
	f.run(f.root, "init", recovered)
	f.run(recovered, "fetch", filepath.Join(result.Archives[0], "history.bundle"), "refs/heads/old")
	f.run(recovered, "checkout", "--detach", oid)
	if got, err := os.ReadFile(filepath.Join(recovered, "unique")); err != nil || string(got) != "unpublished work\n" {
		t.Fatalf("deleted branch unrecoverable: %q %v", got, err)
	}
}
