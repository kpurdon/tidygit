package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetachedSquashCompletionDeletesWorktree(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "detached")
	f.run(f.dir, "worktree", "add", "--detach", path)
	f.write(filepath.Join(path, "tracked"), "first change\n")
	f.run(path, "commit", "-am", "first")
	f.write(filepath.Join(path, "extra"), "second change\n")
	f.run(path, "add", "extra")
	f.run(path, "commit", "-m", "second")
	tip := f.run(path, "rev-parse", "HEAD")
	f.run(f.dir, "merge", "--squash", tip)
	f.run(f.dir, "commit", "-m", "squashed PR")
	merged := f.run(f.dir, "rev-parse", "HEAD")
	f.run(f.dir, "push", "origin", "main")
	// The PR was rebased before merge, so its final head differs from the local snapshot.
	pr := fmt.Sprintf(`{"number":42,"headRefName":"feature","headRefOid":"%s","state":"MERGED","mergeCommit":{"oid":"%s"},"headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"audit"}}`, merged, merged)
	f.write(f.prs, "["+pr+"]")
	graph := fmt.Sprintf(`{"data":{"repository":{"object":{"associatedPullRequests":{"nodes":[{"number":42,"headRefName":"feature","headRefOid":"%s","state":"MERGED","mergeCommit":{"oid":"%s"},"headRepository":{"name":"repo","owner":{"login":"audit"}}}],"pageInfo":{"hasNextPage":false}}}}}}`, merged, merged)
	graphPath := filepath.Join(f.root, "graph.json")
	f.write(graphPath, graph)
	t.Setenv("TEST_GRAPH", graphPath)
	f.write(filepath.Join(f.root, "bin", "gh"), "#!/bin/sh\nif [ \"$1\" = api ]; then cat \"$TEST_GRAPH\"; else cat \"$TEST_PRS\"; fi\n")
	wt := Worktree{Path: path, OID: tip}
	_, complete, err := detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || !complete {
		t.Fatalf("squash completion: complete=%v err=%v", complete, err)
	}
	missing := strings.Repeat("f", 40)
	f.write(f.prs, "["+strings.ReplaceAll(pr, merged, missing)+"]")
	f.write(graphPath, strings.ReplaceAll(graph, merged, missing))
	_, complete, err = detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || complete {
		t.Fatalf("unfetched merge must remain unverified without failing preview: %v %v", complete, err)
	}
	f.write(f.prs, "["+pr+"]")
	f.write(graphPath, graph)
	f.write(filepath.Join(path, "extra"), "unmerged addition\n")
	f.run(path, "commit", "-am", "unmerged local change")
	wt.OID = f.run(path, "rev-parse", "HEAD")
	_, complete, err = detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || complete {
		t.Fatalf("extra local commit accepted as complete: %v, %v", complete, err)
	}
	f.run(path, "reset", "--hard", tip)
	f.write(filepath.Join(path, "ignored"), "local environment\n")
	result := cleanFixture(t, f, false)
	if result.WorktreesRemoved != 1 {
		t.Fatalf("squash cleanup: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
}

func TestDetachedAncestorAndOpenPRProtection(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "detached")
	f.run(f.dir, "worktree", "add", "--detach", path)
	wt := Worktree{Path: path, OID: f.run(path, "rev-parse", "HEAD")}
	_, complete, err := detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || !complete {
		t.Fatalf("ancestor completion: %v %v", complete, err)
	}
	f.pullRequests("OPEN", map[string]string{"feature": wt.OID})
	graph := fmt.Sprintf(`{"data":{"repository":{"object":{"associatedPullRequests":{"nodes":[{"number":1,"headRefName":"feature","headRefOid":"%s","state":"OPEN","headRepository":{"name":"repo","owner":{"login":"audit"}}}],"pageInfo":{"hasNextPage":false}}}}}}`, wt.OID)
	graphPath := filepath.Join(f.root, "graph.json")
	f.write(graphPath, graph)
	t.Setenv("TEST_GRAPH", graphPath)
	f.write(filepath.Join(f.root, "bin", "gh"), "#!/bin/sh\nif [ \"$1\" = api ]; then cat \"$TEST_GRAPH\"; else cat \"$TEST_PRS\"; fi\n")
	pr, complete, err := detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || complete || pr.State != "OPEN" {
		t.Fatalf("open PR failed to protect integrated tip: %+v %v %v", pr, complete, err)
	}
}

func TestIntegratedBranchesRejectChangedDiscoveryTip(t *testing.T) {
	f := newCleanupFixture(t)
	f.run(f.dir, "switch", "-c", "candidate")
	f.run(f.dir, "commit", "--allow-empty", "-m", "unique history")
	oid := f.run(f.dir, "rev-parse", "HEAD")
	f.run(f.dir, "switch", "main")
	f.run(f.dir, "branch", "-f", "candidate", "main")
	integrated, err := integratedBranches(t.Context(), f.repo, "main", []Branch{{Name: "candidate", OID: oid, LocalRepo: "github.com/audit/repo"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if integrated["candidate"] {
		t.Fatal("changed branch tip accepted as the discovered history")
	}
}

func TestIntegratedBranchesIgnoreRewrittenAncestry(t *testing.T) {
	for _, rewrite := range []string{"replace", "graft", "environment-graft"} {
		t.Run(rewrite, func(t *testing.T) {
			f := newCleanupFixture(t)
			f.run(f.dir, "switch", "-c", "unique")
			f.run(f.dir, "commit", "--allow-empty", "-m", "unique history")
			tip := f.run(f.dir, "rev-parse", "HEAD")
			f.run(f.dir, "switch", "main")
			base := f.run(f.dir, "rev-parse", "HEAD")
			if rewrite == "replace" {
				fake := f.run(f.dir, "commit-tree", "HEAD^{tree}", "-p", tip, "-m", "rewritten base")
				f.run(f.dir, "replace", base, fake)
			} else {
				path := filepath.Join(f.repo.CommonDir, "info", "grafts")
				if rewrite == "environment-graft" {
					path = filepath.Join(f.dir, "custom-grafts")
					t.Setenv("GIT_GRAFT_FILE", "custom-grafts")
				}
				f.write(path, base+" "+tip+"\n")
			}
			// The local rewrite makes ordinary Git ancestry claim integration.
			f.run(f.dir, "merge-base", "--is-ancestor", tip, "origin/main")
			integrated, err := integratedBranches(t.Context(), f.repo, "main", []Branch{{Name: "unique", OID: tip, LocalRepo: "github.com/audit/repo"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if integrated["unique"] {
				t.Fatal("rewritten ancestry authorized removal of unique history")
			}
		})
	}
}
