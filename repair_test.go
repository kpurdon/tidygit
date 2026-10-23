package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func repairFixture(t *testing.T, agent, action, failure string) (*cleanupFixture, options) {
	t.Helper()
	f := newCleanupFixture(t)
	f.run(f.dir, "config", "tidygit.agent", agent)
	t.Setenv("TEST_REPAIR_CALLS", filepath.Join(f.root, "agent-calls"))
	t.Setenv("TEST_FETCH_CALLS", filepath.Join(f.root, "fetch-calls"))
	t.Setenv("TEST_FETCH_FAILURE", failure)
	t.Setenv("TEST_REPAIR_AGENT", agent)
	t.Setenv("TEST_REPAIR_PLAN", action)
	bin := filepath.Join(f.root, "bin")
	f.write(filepath.Join(bin, "git"), `#!/bin/sh
if [ "$1" = remote ] && [ "$2" = get-url ]; then exec "$TEST_REAL_GIT" config --get "remote.$3.url"; fi
if [ "$1" = fetch ]; then
  first=no
  if [ ! -f "$TEST_FETCH_CALLS" ]; then first=yes; fi
  echo fetch >> "$TEST_FETCH_CALLS"
  if [ "$TEST_FETCH_FAILURE" = always ] || { [ "$TEST_FETCH_FAILURE" = once ] && [ "$first" = yes ]; }; then
    echo 'transient fetch failure' >&2
    exit 1
  fi
fi
exec "$TEST_REAL_GIT" "$@"
`)
	f.write(filepath.Join(bin, agent), `#!/bin/sh
echo agent >> "$TEST_REPAIR_CALLS"
cat >/dev/null
if [ -n "$TEST_REPAIR_MUTATE" ]; then printf 'concurrent edit\n' > "$TEST_REPAIR_MUTATE"; fi
if [ "$TEST_REPAIR_PLAN" = wait ]; then exec sleep 30; fi
if [ "$TEST_REPAIR_AGENT" = claude ]; then
  printf '{"is_error":false,"structured_output":%s}\n' "$TEST_REPAIR_PLAN"
else
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --output-last-message ]; then
      shift
      printf '%s\n' "$TEST_REPAIR_PLAN" > "$1"
      exit 0
    fi
    shift
  done
  exit 2
fi
`)
	if err := os.Chmod(filepath.Join(bin, agent), 0755); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.Auto, opts.Fix, opts.StaleDays = true, true, 0
	return f, opts
}

func repairCallCount(t *testing.T, env string) int {
	t.Helper()
	data, err := os.ReadFile(os.Getenv(env))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(data)))
}

const retryPlan = `{"action":"retry","remote":"","reason":"Transient fetch failure"}`

func TestRepairRetryProvidersAndMarker(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			f, opts := repairFixture(t, agent, retryPlan, "once")
			result, err := clean(t.Context(), f.repo, opts)
			if err != nil || len(result.Errors) != 0 || !result.Repaired {
				t.Fatalf("repair failed: %+v, %v", result, err)
			}
			if repairCallCount(t, "TEST_REPAIR_CALLS") != 1 || repairCallCount(t, "TEST_FETCH_CALLS") != 2 {
				t.Fatal("expected one agent attempt and one fresh fetch after the failure")
			}
			prompt, err := os.ReadFile(filepath.Join(result.RepairLog, "prompt.txt"))
			if err != nil || !strings.Contains(string(prompt), f.dir) || !strings.Contains(string(prompt), "transient fetch failure") {
				t.Fatalf("missing failure transcript: %s, %v", prompt, err)
			}
			result, err = clean(t.Context(), f.repo, opts)
			if err != nil || !result.Unchanged || repairCallCount(t, "TEST_REPAIR_CALLS") != 1 {
				t.Fatalf("verified repair did not cache successful state: %+v, %v", result, err)
			}
		})
	}
}

func TestRepairFailureDoesNotLoopOrCache(t *testing.T) {
	f, opts := repairFixture(t, "codex", retryPlan, "always")
	result, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(result.Errors) == 0 || result.Repaired {
		t.Fatalf("persistent failure disappeared: %+v, %v", result, err)
	}
	if repairCallCount(t, "TEST_REPAIR_CALLS") != 1 || repairCallCount(t, "TEST_FETCH_CALLS") != 2 {
		t.Fatal("repair retried more than once")
	}
	if err := cleanAll(t.Context(), f.root, opts); err == nil {
		t.Fatal("persistent failure returned success from all")
	}
	if repairCallCount(t, "TEST_REPAIR_CALLS") != 2 || repairCallCount(t, "TEST_FETCH_CALLS") != 4 {
		t.Fatal("failed repair cached state or retried repeatedly")
	}
}

func TestRepairOptInAndDryRun(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "dry-run"}[dryRun], func(t *testing.T) {
			f, opts := repairFixture(t, "codex", retryPlan, "always")
			opts.Fix, opts.DryRun = dryRun, dryRun
			result, err := clean(t.Context(), f.repo, opts)
			if err != nil || result.Repaired || repairCallCount(t, "TEST_REPAIR_CALLS") != 0 {
				t.Fatalf("unexpected repair: %+v, %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(f.root, "state", "tidygit", "repairs")); !os.IsNotExist(err) {
				t.Fatalf("repair transcript created without execution: %v", err)
			}
		})
	}
}

func TestRepairRejectsUnsupportedAction(t *testing.T) {
	f, opts := repairFixture(t, "codex", `{"action":"reset","remote":"","reason":"Discard local history"}`, "once")
	before := f.run(f.dir, "rev-parse", "HEAD")
	result, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(result.Errors) == 0 || result.Repaired || repairCallCount(t, "TEST_FETCH_CALLS") != 1 {
		t.Fatalf("unsupported plan ran: %+v, %v", result, err)
	}
	if f.run(f.dir, "rev-parse", "HEAD") != before {
		t.Fatal("unsupported plan changed local history")
	}
}

func TestRepairRejectsChangedRepository(t *testing.T) {
	f, opts := repairFixture(t, "codex", retryPlan, "once")
	t.Setenv("TEST_REPAIR_MUTATE", filepath.Join(f.dir, "tracked"))
	result, err := clean(t.Context(), f.repo, opts)
	if err != nil || result.Repaired || !strings.Contains(strings.Join(result.Errors, "\n"), "state changed") {
		t.Fatalf("concurrent edit did not stop repair: %+v, %v", result, err)
	}
	if repairCallCount(t, "TEST_FETCH_CALLS") != 1 {
		t.Fatal("cleanup retried despite concurrent changes")
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "tracked"))
	if err != nil || string(data) != "concurrent edit\n" {
		t.Fatalf("concurrent edit lost: %q, %v", data, err)
	}
}

func TestRepairPreservesCallerWorktree(t *testing.T) {
	f, opts := repairFixture(t, "codex", retryPlan, "once")
	path := filepath.Join(f.root, "caller")
	oid := f.branch("merged", path)
	f.pullRequests("MERGED", map[string]string{"merged": oid})
	f.repo.ProtectedPath = canonicalPath(path)
	result, err := clean(t.Context(), f.repo, opts)
	if err != nil || len(result.Errors) != 0 || !result.Repaired {
		t.Fatalf("repair failed: %+v, %v", result, err)
	}
	f.assertBranch("merged", true)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retry removed caller worktree: %v", err)
	}
}

func TestRepairDoesNotResolveConflictsOrOperations(t *testing.T) {
	for _, kind := range []string{"ff", "in-progress"} {
		t.Run(kind, func(t *testing.T) {
			f, opts := repairFixture(t, "codex", retryPlan, "")
			if kind == "ff" {
				f.write(filepath.Join(f.dir, "tracked"), "remote\n")
				f.run(f.dir, "commit", "-am", "remote")
				f.run(f.dir, "push")
				f.run(f.dir, "reset", "--hard", "HEAD^")
				f.write(filepath.Join(f.dir, "tracked"), "local\n")
				f.run(f.dir, "commit", "-am", "local")
			} else {
				f.write(filepath.Join(f.dir, ".git", "MERGE_HEAD"), f.run(f.dir, "rev-parse", "HEAD")+"\n")
			}
			result, err := clean(t.Context(), f.repo, opts)
			if err != nil || len(result.Errors) == 0 || repairCallCount(t, "TEST_REPAIR_CALLS") != 0 {
				t.Fatalf("protected failure invoked repair: %+v, %v", result, err)
			}
		})
	}
}

func TestRepairPrunesOnlyConventionalRemoteRefs(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "conventional", true: "custom"}[custom], func(t *testing.T) {
			f, opts := repairFixture(t, "codex", `{"action":"prune_remote_refs","remote":"origin","reason":"Stale remote-tracking refs block fetch"}`, "once")
			oid := f.branch("local-only", "")
			f.run(f.dir, "tag", "local-tag")
			f.run(f.dir, "update-ref", "refs/remotes/origin/stale", oid)
			f.run(f.dir, "config", "fetch.pruneTags", "true")
			f.run(f.dir, "config", "remote.origin.pruneTags", "true")
			if custom {
				f.run(f.dir, "config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/heads/*")
			}
			// Isolate the repair action from the subsequent normal fetch, which honors the user's pruneTags setting.
			original := repoResult{Name: "repo", Errors: []string{"fetch failed"}, repairStage: "fetching"}
			t.Setenv("TEST_FETCH_FAILURE", "always")
			result, err := repairRepository(t.Context(), f.repo, opts, "codex", original)
			if err != nil || result.Repaired {
				t.Fatalf("expected the subsequent fetch to fail: %+v, %v", result, err)
			}
			if f.run(f.dir, "rev-parse", "refs/heads/local-only") != oid || f.run(f.dir, "rev-parse", "refs/tags/local-tag") != oid {
				t.Fatal("repair pruned local branches or tags")
			}
			refs := f.run(f.dir, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/stale")
			if custom && refs == "" || !custom && refs != "" {
				t.Fatalf("wrong stale-ref outcome for custom=%v: %q", custom, refs)
			}
			if custom && repairCallCount(t, "TEST_FETCH_CALLS") != 0 {
				t.Fatal("custom refspec was accepted for repair")
			}
		})
	}
}

func TestRepairAgentTimeoutAndCancellation(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancellation"}[cancelParent], func(t *testing.T) {
			_, _ = repairFixture(t, "codex", "wait", "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			timeout, expected := 50*time.Millisecond, context.DeadlineExceeded
			if cancelParent {
				timeout, expected = time.Minute, context.Canceled
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			_, err := runRepairAgent(ctx, "codex", t.TempDir(), "diagnose", timeout)
			if !errors.Is(err, expected) {
				t.Fatalf("expected %v, got %v", expected, err)
			}
		})
	}
}
