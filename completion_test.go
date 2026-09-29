package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDetachedSquashCompletionArchivesLocalHistory(t *testing.T) {
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
	_, complete, archive, err := detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || !complete || !archive {
		t.Fatalf("squash completion must preserve history: complete=%v archive=%v err=%v", complete, archive, err)
	}
	f.write(filepath.Join(path, "extra"), "unmerged addition\n")
	f.run(path, "commit", "-am", "unmerged local change")
	wt.OID = f.run(path, "rev-parse", "HEAD")
	_, complete, _, err = detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || complete {
		t.Fatalf("extra local commit accepted as complete: %v, %v", complete, err)
	}
	f.run(path, "reset", "--hard", tip)
	f.write(filepath.Join(path, "ignored"), "local environment\n")
	result := cleanFixture(t, f, false)
	if result.WorktreesRemoved != 1 || len(result.Archives) != 1 {
		t.Fatalf("squash cleanup did not archive and remove: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
	archivePath := result.Archives[0]
	f.run(f.dir, "bundle", "verify", filepath.Join(archivePath, "history.bundle"))
	data, err := os.ReadFile(filepath.Join(archivePath, "files", "detached", "ignored"))
	if err != nil || string(data) != "local environment\n" {
		t.Fatalf("ignored environment missing: %q, %v", data, err)
	}
}

func TestDetachedAncestorAndOpenPRProtection(t *testing.T) {
	f := newCleanupFixture(t)
	path := filepath.Join(f.root, "detached")
	f.run(f.dir, "worktree", "add", "--detach", path)
	wt := Worktree{Path: path, OID: f.run(path, "rev-parse", "HEAD")}
	_, complete, archive, err := detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || !complete || archive {
		t.Fatalf("ancestor completion: %v %v %v", complete, archive, err)
	}
	f.pullRequests("OPEN", map[string]string{"feature": wt.OID})
	graph := fmt.Sprintf(`{"data":{"repository":{"object":{"associatedPullRequests":{"nodes":[{"number":1,"headRefName":"feature","headRefOid":"%s","state":"OPEN","headRepository":{"name":"repo","owner":{"login":"audit"}}}],"pageInfo":{"hasNextPage":false}}}}}}`, wt.OID)
	graphPath := filepath.Join(f.root, "graph.json")
	f.write(graphPath, graph)
	t.Setenv("TEST_GRAPH", graphPath)
	f.write(filepath.Join(f.root, "bin", "gh"), "#!/bin/sh\nif [ \"$1\" = api ]; then cat \"$TEST_GRAPH\"; else cat \"$TEST_PRS\"; fi\n")
	pr, complete, _, err := detachedCompletion(t.Context(), f.repo, wt, "main", defaultOptions())
	if err != nil || complete || pr.State != "OPEN" {
		t.Fatalf("open PR failed to protect integrated tip: %+v %v %v", pr, complete, err)
	}
}
