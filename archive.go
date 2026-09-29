package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ignoredPaths returns relative ignored entries, collapsing ignored-only directories.
func ignoredPaths(ctx context.Context, r repository, worktreePath string) ([]string, error) {
	out, err := r.git(ctx, "-C", worktreePath, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil, err
	}
	protected, err := r.git(ctx, "-C", worktreePath, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var protectedPaths []string
	for name := range strings.SplitSeq(protected, "\x00") {
		if name != "" {
			protectedPaths = append(protectedPaths, filepath.FromSlash(name))
		}
	}
	slices.Sort(protectedPaths)
	var paths []string
	for name := range strings.SplitSeq(out, "\x00") {
		if name == "" {
			continue
		}
		name = filepath.FromSlash(strings.TrimSuffix(name, "/"))
		if err := validArchivePath(name); err != nil {
			return nil, err
		}
		if other := archivePathOverlap(protectedPaths, name); other != "" {
			return nil, fmt.Errorf("ignored entry %q overlaps tracked or untracked path %q", name, other)
		}
		paths = append(paths, name)
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// archivePathOverlap checks a sorted inventory without rescanning every tracked file.
func archivePathOverlap(paths []string, name string) string {
	for parent := name; parent != "."; parent = filepath.Dir(parent) {
		if _, found := slices.BinarySearch(paths, parent); found {
			return parent
		}
	}
	prefix := name + string(filepath.Separator)
	i, _ := slices.BinarySearch(paths, prefix)
	if i < len(paths) && strings.HasPrefix(paths[i], prefix) {
		return paths[i]
	}
	return ""
}

func validArchivePath(name string) error {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name || name == "." {
		return fmt.Errorf("unsafe archive path %q", name)
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("refusing to archive Git metadata %q", name)
		}
	}
	return nil
}

func archiveSourceInfo(root *os.Root, name string) (os.FileInfo, error) {
	if err := validArchivePath(name); err != nil {
		return nil, err
	}
	parts := strings.Split(name, string(filepath.Separator))
	current := "."
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if i == len(parts)-1 {
			return info, nil
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("archive path has non-directory or symlink parent %q", current)
		}
	}
	return nil, fmt.Errorf("empty archive path")
}

// archiveIgnored relocates entries without following their symlinks. On failure,
// the returned archive remains recoverable and cleanup must stop. Rename requires
// the archive and worktree to share a filesystem; no copy-and-delete fallback runs.
func archiveIgnored(ctx context.Context, worktreePath, archiveBase string, paths []string) (archive string, err error) {
	if len(paths) == 0 {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source, err := filepath.EvalSymlinks(worktreePath)
	if err != nil {
		return "", err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", err
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return "", err
	}
	defer sourceRoot.Close()
	// Resolve an existing ancestor before creating the base, including symlinked parents.
	base, err := filepath.Abs(archiveBase)
	if err != nil {
		return "", err
	}
	ancestor := base
	var suffix []string
	for {
		real, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr == nil {
			base = filepath.Join(append([]string{real}, suffix...)...)
			break
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", resolveErr
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
	if rel, relErr := filepath.Rel(source, base); relErr == nil && (rel == "." || filepath.IsLocal(rel)) {
		return "", fmt.Errorf("archive directory must be outside worktree %q", source)
	}
	paths = slices.Clone(paths)
	slices.Sort(paths)
	for i, name := range paths {
		if _, err := archiveSourceInfo(sourceRoot, name); err != nil {
			return "", err
		}
		if prior := archivePathOverlap(paths[:i], name); prior != "" {
			return "", fmt.Errorf("overlapping archive paths %q and %q", prior, name)
		}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	archive, err = os.MkdirTemp(base, "worktree-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("ignored-file archival stopped; recover moved entries from %q/files (original worktree %q): %w", archive, source, err)
		}
	}()
	var manifest strings.Builder
	fmt.Fprintf(&manifest, "Original worktree (Go-quoted): %q\n\nFiles under files/ preserve their original relative paths, bytes, modes, and symlinks.\nTo recover, copy or move the desired entries back to the original worktree or another directory.\nDo not overwrite existing files. Some entries may remain in the original worktree if archival stopped.\n\nPlanned relative paths (Go-quoted):\n", source)
	for _, name := range paths {
		fmt.Fprintf(&manifest, "%q\n", name)
	}
	if err = os.WriteFile(filepath.Join(archive, "README.txt"), []byte(manifest.String()), 0o600); err != nil {
		return archive, err
	}
	if err = os.Mkdir(filepath.Join(archive, "files"), 0o700); err != nil {
		return archive, err
	}
	destinationRoot, err := os.OpenRoot(filepath.Join(archive, "files"))
	if err != nil {
		return archive, err
	}
	defer destinationRoot.Close()
	for _, name := range paths {
		if err = ctx.Err(); err != nil {
			return archive, err
		}
		before, checkErr := archiveSourceInfo(sourceRoot, name)
		if checkErr != nil {
			return archive, checkErr
		}
		if err = destinationRoot.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			return archive, err
		}
		if err = moveArchiveEntry(sourceRoot, destinationRoot, name); err != nil {
			return archive, fmt.Errorf("move %q (archive must be on the same filesystem): %w", name, err)
		}
		after, checkErr := destinationRoot.Lstat(name)
		if checkErr != nil {
			return archive, checkErr
		}
		if !os.SameFile(before, after) {
			return archive, fmt.Errorf("entry %q changed during archival", name)
		}
		if _, checkErr = sourceRoot.Lstat(name); !errors.Is(checkErr, os.ErrNotExist) {
			return archive, fmt.Errorf("source %q reappeared or could not be checked after archival: %v", name, checkErr)
		}
	}
	return archive, nil
}

// Keep parent descriptors open across rename so path replacement cannot redirect
// the move through a symlink outside either root.
func moveArchiveEntry(sourceRoot, destinationRoot *os.Root, name string) error {
	sourceParent, err := sourceRoot.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	destinationParent, err := destinationRoot.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer destinationParent.Close()
	return renamePinnedArchive(sourceParent, destinationParent, filepath.Base(name))
}
