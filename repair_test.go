package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func repairFixture(t *testing.T, agent, report, failure string) (*cleanupFixture, options) {
	t.Helper()
	f := newCleanupFixture(t)
	f.run(f.dir, "config", "tidygit.agent", agent)
	t.Setenv("TEST_REPAIR_CALLS", filepath.Join(f.root, "agent-calls"))
	t.Setenv("TEST_FETCH_CALLS", filepath.Join(f.root, "fetch-calls"))
	t.Setenv("TEST_FETCH_FAILURE", failure)
	t.Setenv("TEST_REPAIR_AGENT", agent)
	t.Setenv("TEST_REPAIR_REPORT", report)
	t.Setenv("TEST_REPAIR_MCP", "")
	t.Setenv("TEST_REPAIR_REPO", f.dir)
	t.Setenv("TEST_REPAIR_ARGS", filepath.Join(f.root, "agent-args"))
	t.Setenv("TEST_REPAIR_CHANGE", "")
	bin := filepath.Join(f.root, "bin")
	f.write(filepath.Join(bin, "git"), `#!/bin/sh
if [ "$1" = remote ] && [ "$2" = get-url ]; then exec "$TEST_REAL_GIT" config --get "remote.$3.url"; fi
if [ "$1" = fetch ]; then
  first=no
  if [ ! -f "$TEST_FETCH_CALLS" ]; then first=yes; fi
  echo fetch >> "$TEST_FETCH_CALLS"
  if [ "$TEST_FETCH_FAILURE" = always ] || { [ "$TEST_FETCH_FAILURE" = once ] && [ "$first" = yes ]; } || { [ "$TEST_FETCH_FAILURE" = remote-ref ] && "$TEST_REAL_GIT" show-ref --verify --quiet refs/remotes/origin/stale; }; then
    echo 'transient fetch failure' >&2
    exit 1
  fi
fi
exec "$TEST_REAL_GIT" "$@"
`)
	f.write(filepath.Join(bin, agent), `#!/bin/sh
disabled=false
transport=false
isolated=false
notify=false
for arg in "$@"; do
  if [ "$arg" = 'project_root_markers=[]' ]; then isolated=true; fi
  if [ "$arg" = 'notify=[]' ]; then notify=true; fi
  if [ "$arg" = 'mcp_servers.tools_remote.enabled=false' ]; then disabled=true; fi
  if [ "$TEST_REPAIR_MCP" = http ]; then
    if [ "$arg" = 'mcp_servers.tools_remote.url="http://127.0.0.1:1"' ]; then transport=true; fi
  elif [ "$arg" = 'mcp_servers.tools_remote.command="tidygit-disabled-mcp"' ]; then transport=true
  fi
done
if [ "$TEST_REPAIR_AGENT" = codex ] && { [ "$isolated" != true ] || [ "$notify" != true ]; }; then exit 8; fi
if [ "$1" = mcp ]; then
  case "$TEST_REPAIR_MCP" in
    enabled) printf '[{"name":"tools_remote","enabled":%s,"transport":{"type":"stdio"}}]\n' "$([ "$disabled" = true ] && echo false || echo true)" ;;
    http) printf '[{"name":"tools_remote","enabled":%s,"transport":{"type":"streamable_http"}}]\n' "$([ "$disabled" = true ] && echo false || echo true)" ;;
    disabled) printf '[{"name":"tools_remote","enabled":false,"transport":{"type":"stdio"}}]\n' ;;
    managed) printf '[{"name":"tools_remote","enabled":true,"transport":{"type":"stdio"}}]\n' ;;
    unsupported) printf '[{"name":"tools.remote","enabled":true,"transport":{"type":"stdio"}}]\n' ;;
    malformed) printf 'not json\n' ;;
    null) printf 'null\n' ;;
    *) printf '[]\n' ;;
  esac
  exit 0
fi
if [ -n "$TEST_REPAIR_MCP" ] && { [ "$disabled" != true ] || [ "$transport" != true ]; }; then exit 7; fi
echo agent >> "$TEST_REPAIR_CALLS"
printf '%s\n' "$@" > "$TEST_REPAIR_ARGS"
cat >/dev/null
case "$TEST_REPAIR_CHANGE" in
  tracked) printf 'concurrent edit\n' > "$TEST_REPAIR_REPO/tracked" ;;
  branch) "$TEST_REAL_GIT" -C "$TEST_REPAIR_REPO" branch agent-created ;;
  config) "$TEST_REAL_GIT" -C "$TEST_REPAIR_REPO" config tidygit.keep agent-kept ;;
  remote)
    oid=$("$TEST_REAL_GIT" -C "$TEST_REPAIR_REPO" rev-parse refs/remotes/origin/stale) || exit 9
    "$TEST_REAL_GIT" -C "$TEST_REPAIR_REPO" update-ref -d refs/remotes/origin/stale "$oid" || exit 9
    ;;
esac
if [ "$TEST_REPAIR_REPORT" = wait ]; then exec sleep 30; fi
if [ "$TEST_REPAIR_REPORT" = wait-detached ] || [ "$TEST_REPAIR_REPORT" = crash-detached ]; then
  trap 'printf INT > "$TEST_REPAIR_SIGNAL"; exit 0' INT
  trap 'printf TERM > "$TEST_REPAIR_SIGNAL"; exit 0' TERM
  "$TEST_REPAIR_CHILD_EXEC" -test.run='^TestRepairDetachedChild$' </dev/null &
  if [ "$TEST_REPAIR_REPORT" = crash-detached ]; then sleep 1; exit 9; fi
  wait
  exit 0
fi
if [ "$TEST_REPAIR_AGENT" = claude ]; then
  printf '{"is_error":false,"structured_output":%s}\n' "$TEST_REPAIR_REPORT"
else
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --output-last-message ]; then
      shift
      printf '%s\n' "$TEST_REPAIR_REPORT" > "$1"
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

const repairedReport = `{"status":"repaired","summary":"Repaired remote metadata"}`

func TestRepairCodexMCPRestrictions(t *testing.T) {
	for _, inventory := range []string{"enabled", "disabled", "http", "managed", "malformed", "unsupported", "null"} {
		t.Run(inventory, func(t *testing.T) {
			f, _ := repairFixture(t, "codex", repairedReport, "")
			t.Setenv("TEST_REPAIR_MCP", inventory)
			report, err := runRepairAgent(t.Context(), "codex", t.TempDir(), "diagnose", time.Second, f.repo)
			blocked := inventory == "managed" || inventory == "malformed" || inventory == "unsupported" || inventory == "null"
			if blocked {
				if err == nil || repairCallCount(t, "TEST_REPAIR_CALLS") != 0 {
					t.Fatalf("unsafe MCP inventory started diagnosis: %v", err)
				}
			} else if err != nil || report.Status != "repaired" || repairCallCount(t, "TEST_REPAIR_CALLS") != 1 {
				t.Fatalf("MCP override missing from diagnosis: %+v, %v", report, err)
			}
		})
	}
}

func TestRepairRetryProvidersAndMarker(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			f, opts := repairFixture(t, agent, repairedReport, "remote-ref")
			oid := f.branch("local-only", "")
			f.run(f.dir, "tag", "local-tag")
			f.run(f.dir, "update-ref", "refs/remotes/origin/stale", oid)
			t.Setenv("TEST_REPAIR_CHANGE", "remote")
			result, err := clean(t.Context(), f.repo, opts)
			if err != nil || len(result.Errors) != 0 || !result.Repaired {
				t.Fatalf("repair failed: %+v, %v", result, err)
			}
			if repairCallCount(t, "TEST_REPAIR_CALLS") != 1 || repairCallCount(t, "TEST_FETCH_CALLS") != 2 {
				t.Fatal("expected one agent attempt and one fresh fetch after the failure")
			}
			if f.run(f.dir, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/stale") != "" {
				t.Fatal("agent did not repair remote metadata")
			}
			if f.run(f.dir, "rev-parse", "refs/heads/local-only") != oid || f.run(f.dir, "rev-parse", "refs/tags/local-tag") != oid {
				t.Fatal("repair changed local branches or tags")
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
	f, opts := repairFixture(t, "codex", repairedReport, "always")
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
			f, opts := repairFixture(t, "codex", repairedReport, "always")
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

func TestRepairRejectsTranscriptInsideWorktree(t *testing.T) {
	f, opts := repairFixture(t, "codex", repairedReport, "once")
	path := filepath.Join(f.root, "linked")
	f.branch("linked", path)
	state := filepath.Join(path, "state")
	t.Setenv("XDG_STATE_HOME", state)
	result, err := clean(t.Context(), f.repo, opts)
	if err != nil || result.Repaired || !strings.Contains(strings.Join(result.Errors, "\n"), "XDG_STATE_HOME") {
		t.Fatalf("transcript location was accepted: %+v, %v", result, err)
	}
	if repairCallCount(t, "TEST_REPAIR_CALLS") != 0 {
		t.Fatal("agent started with transcripts inside a worktree")
	}
	if _, err := os.Stat(filepath.Join(state, "tidygit", "repairs")); !os.IsNotExist(err) {
		t.Fatalf("repair transcript directory was created: %v", err)
	}
}

func TestRepairBlockedReportKeepsFailure(t *testing.T) {
	f, opts := repairFixture(t, "codex", `{"status":"blocked","summary":"Another Git process owns the lock"}`, "once")
	result, err := clean(t.Context(), f.repo, opts)
	if err != nil || result.Repaired || !strings.Contains(strings.Join(result.Errors, "\n"), "Another Git process owns the lock") {
		t.Fatalf("blocked report hid failure or reason: %+v, %v", result, err)
	}
	if repairCallCount(t, "TEST_FETCH_CALLS") != 1 {
		t.Fatal("blocked repair retried cleanup")
	}
}

func TestRepairRejectsChangedLocalState(t *testing.T) {
	for _, change := range []string{"tracked", "branch", "config"} {
		t.Run(change, func(t *testing.T) {
			f, opts := repairFixture(t, "codex", repairedReport, "once")
			t.Setenv("TEST_REPAIR_CHANGE", change)
			result, err := clean(t.Context(), f.repo, opts)
			if err != nil || result.Repaired || !strings.Contains(strings.Join(result.Errors, "\n"), "local work or configuration changed") {
				t.Fatalf("unexpected local change did not stop cleanup: %+v, %v", result, err)
			}
			if repairCallCount(t, "TEST_FETCH_CALLS") != 1 {
				t.Fatal("cleanup retried despite changes to local work")
			}
			switch change {
			case "tracked":
				data, err := os.ReadFile(filepath.Join(f.dir, "tracked"))
				if err != nil || string(data) != "concurrent edit\n" {
					t.Fatalf("edit lost: %q, %v", data, err)
				}
			case "branch":
				f.assertBranch("agent-created", true)
			case "config":
				if f.run(f.dir, "config", "--get", "tidygit.keep") != "agent-kept" {
					t.Fatal("changed configuration was overwritten")
				}
			}
		})
	}
}

func TestRepairPreservesCallerWorktree(t *testing.T) {
	f, opts := repairFixture(t, "codex", repairedReport, "once")
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
			f, opts := repairFixture(t, "codex", repairedReport, "")
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

func TestRepairAgentRootsAndPermissions(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			f, _ := repairFixture(t, agent, repairedReport, "")
			path := filepath.Join(f.root, "linked")
			f.branch("linked", path)
			r, err := discoverRepo(t.Context(), path, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = runRepairAgent(t.Context(), agent, t.TempDir(), "repair", time.Second, r)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(os.Getenv("TEST_REPAIR_ARGS"))
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			var roots []string
			values := map[string]string{}
			for i, arg := range args {
				if i+1 < len(args) {
					values[arg] = args[i+1]
					if arg == "--add-dir" {
						roots = append(roots, args[i+1])
					}
				}
			}
			wantRoots := []string{r.Dir, r.CommonDir, canonicalPath(f.run(path, "rev-parse", "--absolute-git-dir"))}
			if !slices.Equal(roots, wantRoots) {
				t.Fatalf("writable roots = %v, want %v", roots, wantRoots)
			}
			if slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") || slices.Contains(args, "--dangerously-skip-permissions") {
				t.Fatal("repair bypasses agent permission controls")
			}
			if agent == "codex" {
				if !slices.Contains(args, "--approve-for-me") || slices.Contains(args, "features.shell_tool=false") || slices.Contains(args, "features.unified_exec=false") {
					t.Fatal("Codex cannot request permission to repair metadata")
				}
			} else if values["--permission-mode"] != "auto" || values["--tools"] != "Bash,Read,Edit,Write,Glob,Grep" {
				t.Fatalf("Claude repair tools or permissions unavailable: %v", args)
			}
		})
	}
}

func TestRepairAgentTimeoutAndCancellation(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancellation"}[cancelParent], func(t *testing.T) {
			f, _ := repairFixture(t, "codex", "wait", "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			timeout, expected := 50*time.Millisecond, context.DeadlineExceeded
			if cancelParent {
				timeout, expected = time.Minute, context.Canceled
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			_, err := runRepairAgent(ctx, "codex", t.TempDir(), "diagnose", timeout, f.repo)
			if !errors.Is(err, expected) {
				t.Fatalf("expected %v, got %v", expected, err)
			}
		})
	}
}
