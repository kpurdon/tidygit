package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestIgnoredInventoryPreservesTrackedAndUntrackedFiles(t *testing.T) {
	f := newCleanupFixture(t)
	f.write(filepath.Join(f.dir, ".gitignore"), "ignored\ncache/\n*.secret\nempty/\n")
	for _, dir := range []string{"cache", "ignored/nested"} {
		if err := os.MkdirAll(filepath.Join(f.dir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.write(filepath.Join(f.dir, "cache", "tracked"), "tracked")
	f.run(f.dir, "add", "-f", "cache/tracked")
	f.write(filepath.Join(f.dir, "cache", "junk"), "ignored")
	f.write(filepath.Join(f.dir, "ignored", "nested", "data"), "ignored")
	f.write(filepath.Join(f.dir, "loose"), "keep")
	f.write(filepath.Join(f.dir, "line\nbreak.secret"), "ignored")
	if err := os.Mkdir(filepath.Join(f.dir, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := ignoredPaths(t.Context(), f.repo, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cache/junk", "empty", "ignored", "line\nbreak.secret"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("ignored paths = %q, want %q", paths, want)
	}
	archive, err := archiveIgnored(t.Context(), f.dir, filepath.Join(f.root, "archives"), paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tracked", "cache/tracked", "loose", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err != nil {
			t.Fatalf("lost %s: %v", name, err)
		}
	}
	for _, name := range paths {
		if _, err := os.Lstat(filepath.Join(archive, "files", name)); err != nil {
			t.Fatalf("missing archive %q: %v", name, err)
		}
		if _, err := os.Lstat(filepath.Join(f.dir, name)); !os.IsNotExist(err) {
			t.Fatalf("source still exists %q: %v", name, err)
		}
	}
}

func TestArchivePreservesBytesModesAndExternalSymlinks(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("safe archival requires descriptor-relative rename")
	}
	root := t.TempDir()
	source := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(source, "ignored", "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 255, 13, 10, 128}
	name := filepath.Join("ignored", "nested", "binary\nfile")
	if err := os.WriteFile(filepath.Join(source, name), data, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, name), 0o640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("retain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	archive, err := archiveIgnored(t.Context(), source, filepath.Join(root, "archives"), []string{"ignored", "link"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(archive, "files", name))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("bytes: %v %v", got, err)
	}
	info, err := os.Stat(filepath.Join(archive, "files", name))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode: %v %v", info, err)
	}
	target, err := os.Readlink(filepath.Join(archive, "files", "link"))
	if err != nil || target != outside {
		t.Fatalf("symlink: %q %v", target, err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "retain" {
		t.Fatalf("outside modified: %q %v", got, err)
	}
	if info, err := os.Stat(archive); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("archive permissions: %v %v", info, err)
	}
	manifest, err := os.ReadFile(filepath.Join(archive, "README.txt"))
	if err != nil || !strings.Contains(string(manifest), source) {
		t.Fatalf("recovery metadata: %s %v", manifest, err)
	}
}

func TestArchiveRejectsUnsafePathsWithoutMovingFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "worktree")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{{"../outside"}, {".git"}, {"escape/worktree/keep"}, {"keep", "keep"}} {
		if _, err := archiveIgnored(t.Context(), source, filepath.Join(root, "archives"), paths); err == nil {
			t.Fatalf("accepted unsafe %q", paths)
		}
	}
	if _, err := archiveIgnored(t.Context(), source, filepath.Join(source, "archive"), []string{"keep"}); err == nil {
		t.Fatal("accepted archive inside source")
	}
	if err := os.Symlink(source, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveIgnored(t.Context(), source, filepath.Join(root, "alias", "archive"), []string{"keep"}); err == nil {
		t.Fatal("accepted archive symlink into source")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := archiveIgnored(ctx, source, filepath.Join(root, "archives"), []string{"keep"}); err == nil {
		t.Fatal("ignored canceled context")
	}
	if got, err := os.ReadFile(filepath.Join(source, "keep")); err != nil || string(got) != "keep" {
		t.Fatalf("source lost: %q %v", got, err)
	}
}

func TestArchivePartialFailureKeepsEveryEntryRecoverable(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("safe archival requires descriptor-relative rename")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	root := t.TempDir()
	source := filepath.Join(root, "worktree")
	locked := filepath.Join(source, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "locked/second"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	archive, err := archiveIgnored(t.Context(), source, filepath.Join(root, "archives"), []string{"first", "locked/second"})
	if err == nil || archive == "" || !strings.Contains(err.Error(), archive) {
		t.Fatalf("expected recoverable error, archive=%q err=%v", archive, err)
	}
	for _, name := range []string{"first", "locked/second"} {
		data, readErr := os.ReadFile(filepath.Join(source, name))
		if os.IsNotExist(readErr) {
			data, readErr = os.ReadFile(filepath.Join(archive, "files", name))
		}
		if readErr != nil || string(data) != name {
			t.Fatalf("lost %q: %q %v", name, data, readErr)
		}
	}
}

func TestArchiveOverlapDetectsAncestorsAndDescendants(t *testing.T) {
	paths := []string{"dir/file", "parent", "parent-neighbor", "same"}
	for i := range paths {
		paths[i] = filepath.FromSlash(paths[i])
	}
	slices.Sort(paths)
	for _, name := range []string{"dir", "parent/nested/file", "same"} {
		if got := archivePathOverlap(paths, filepath.FromSlash(name)); got == "" {
			t.Errorf("missed overlap for %q", name)
		}
	}
	for _, name := range []string{"directory", "parent-new", "same-prefix"} {
		if got := archivePathOverlap(paths, filepath.FromSlash(name)); got != "" {
			t.Errorf("false overlap for %q: %q", name, got)
		}
	}
	for _, name := range []string{"nested/.git/config", `nested\.git\config`} {
		if err := validArchivePath(name); err == nil {
			t.Errorf("accepted Git metadata %q", name)
		}
	}
}
