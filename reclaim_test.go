package main

import (
	"os"
	"path/filepath"
	"testing"
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
			if len(result.Archives) != 0 {
				t.Fatalf("default cleanup created archives: %+v", result)
			}
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

func TestCompletedRecoverableSubmoduleWorktreeRemovedWithoutArchive(t *testing.T) {
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
	if result.WorktreesRemoved != 1 || result.BranchesDeleted != 1 || len(result.Archives) != 0 {
		t.Fatalf("recoverable submodule worktree not reclaimed: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("checkout still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "child", "tracked")); err != nil {
		t.Fatalf("primary child changed: %v", err)
	}
}
