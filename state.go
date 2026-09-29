package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func stateDirectory() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" && filepath.IsAbs(base) {
		return filepath.Join(base, "tidygit"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "tidygit"), nil
}

// localFingerprint deliberately consults no remote services. File metadata detects
// ordinary edits, including ignored files, without reading build-cache contents.
// A content edit preserving both size and modification time requires --force.
func localFingerprint(ctx context.Context, r repository, opts options) (string, error) {
	return localFingerprintAt(ctx, r, opts, time.Now())
}

func localFingerprintAt(ctx context.Context, r repository, opts options, now time.Time) (string, error) {
	h := sha256.New()
	fingerprintField(h, fmt.Sprintf("v2:%s:%s:%s:%t:%t:%d:%s:%d", version, canonicalPath(r.CommonDir), canonicalPath(r.ProtectedPath), opts.Auto, opts.Rebase, opts.PRLimit, opts.PRAuthor, opts.StaleDays))
	for _, args := range [][]string{{"for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)"}, {"config", "--null", "--list", "--show-origin"}} {
		out, err := r.git(ctx, args...)
		if err != nil {
			return "", err
		}
		fingerprintField(h, out)
	}
	// Repository-local ignore and attribute rules can change classification without
	// changing refs or the worktree files themselves.
	for _, name := range []string{"info/exclude", "info/attributes"} {
		path := filepath.Join(r.CommonDir, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		fingerprintField(h, name)
		fingerprintField(h, string(data))
	}
	for _, rule := range []struct{ key, fallback string }{{"core.excludesFile", "ignore"}, {"core.attributesFile", "attributes"}} {
		out, err := r.git(ctx, "config", "--null", "--path", "--default", "", "--get", rule.key)
		if err != nil {
			return "", err
		}
		path := strings.TrimSuffix(out, "\x00")
		if path == "" {
			base := os.Getenv("XDG_CONFIG_HOME")
			if base == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return "", err
				}
				base = filepath.Join(home, ".config")
			}
			path = filepath.Join(base, "git", rule.fallback)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.Dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		fingerprintField(h, path)
		fingerprintField(h, string(data))
	}
	worktrees, err := r.worktrees(ctx)
	if err != nil {
		return "", err
	}
	for _, wt := range worktrees {
		stale, err := staleWorktree(ctx, r, wt, opts.StaleDays, now)
		if err != nil {
			return "", err
		}
		fingerprintField(h, fmt.Sprint(stale))
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fingerprintField(h, fmt.Sprintf("%q:%q:%q:%t:%t", wt.Path, wt.Branch, wt.OID, wt.Locked, wt.Prunable))
		wr := r
		wr.Dir = wt.Path
		admin, err := wr.gitDir(ctx)
		if err != nil {
			return "", err
		}
		// Index entries, not index timestamps/stat-cache extensions, describe staging.
		index, err := wr.git(ctx, "ls-files", "--stage", "-v", "-z")
		if err != nil {
			return "", err
		}
		fingerprintField(h, index)
		for _, name := range []string{"HEAD", "config.worktree", "rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer", "BISECT_START"} {
			path := filepath.Join(admin, name)
			if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				fingerprintField(h, name+":absent")
				continue
			} else if err != nil {
				return "", err
			}
			fingerprintField(h, name)
			if err := fingerprintTree(ctx, h, path, true); err != nil {
				return "", err
			}
		}
		if err := fingerprintTree(ctx, h, wt.Path, false); err != nil {
			return "", err
		}
	}
	if opts.StaleDays > 0 {
		checkedOut := make(map[string]bool, len(worktrees))
		for _, wt := range worktrees {
			checkedOut[wt.Branch] = true
		}
		branches, err := r.branches(ctx)
		if err != nil {
			return "", err
		}
		for _, branch := range branches {
			if checkedOut[branch.Name] {
				continue
			}
			stale, err := staleBranch(ctx, r, branch, opts.StaleDays, now)
			if err != nil {
				return "", err
			}
			fingerprintField(h, branch.Name)
			fingerprintField(h, fmt.Sprint(stale))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func fingerprintField(h hash.Hash, value string) { fmt.Fprintf(h, "%d:%s", len(value), value) }

func fingerprintTree(ctx context.Context, h hash.Hash, root string, contents bool) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if !contents && entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fingerprintField(h, rel)
		fingerprintField(h, info.Mode().String())
		if entry.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fingerprintField(h, target)
		} else if contents && info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fingerprintField(h, string(data))
		} else {
			fingerprintField(h, fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano()))
		}
		return nil
	})
}

func runMarkerPath(r repository) (string, error) {
	base, err := stateDirectory()
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(canonicalPath(r.CommonDir)))
	return filepath.Join(base, "runs", fmt.Sprintf("%x", key)), nil
}

func readRunMarker(r repository, fingerprint string) (bool, error) {
	path, err := runMarkerPath(r)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(data)) == fingerprint, nil
}

func writeRunMarker(r repository, fingerprint string) error {
	path, err := runMarkerPath(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".run-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(fingerprint + "\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func invalidateRunMarker(r repository) error {
	path, err := runMarkerPath(r)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
