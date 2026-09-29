//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveRenamePinsParentsAcrossSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	outside := filepath.Join(root, "outside")
	for _, path := range []string{source, destination, outside} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{filepath.Join(source, "entry"): "ignored bytes", filepath.Join(outside, "entry"): "outside bytes"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sourceParent, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceParent.Close()
	destinationParent, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationParent.Close()
	for _, path := range []string{source, destination} {
		if err := os.Rename(path, path+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := renamePinnedArchive(sourceParent, destinationParent, "entry"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{filepath.Join(destination+"-original", "entry"): "ignored bytes", filepath.Join(outside, "entry"): "outside bytes"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s: got %q, want %q, err %v", path, got, want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(source+"-original", "entry")); !os.IsNotExist(err) {
		t.Fatalf("original entry was not moved: %v", err)
	}
}
