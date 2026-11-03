package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// operationProtection includes branches temporarily detached by rebase or bisect,
// and branches an interactive rebase will update in other worktrees.
type operationProtection struct {
	worktrees map[string]string
	branches  []Worktree
}

func (p operationProtection) branchReason(r repository, name string) (string, error) {
	path, err := r.checkedOutWorktree(name, p.branches)
	if err != nil || path == "" {
		return "", err
	}
	return p.worktrees[canonicalPath(path)], nil
}

func (r repository) operationProtection(ctx context.Context, worktrees []Worktree) (operationProtection, error) {
	protection := operationProtection{worktrees: map[string]string{}}
	for _, wt := range worktrees {
		linked := r
		linked.Dir = wt.Path
		state, err := linked.inProgress(ctx)
		if err != nil {
			return protection, err
		}
		if state == "" {
			continue
		}
		protection.worktrees[canonicalPath(wt.Path)] = fmt.Sprintf("%s in progress in %s", state, wt.Path)
		seen := map[string]bool{}
		protectRef := func(ref string) error {
			name, ok := strings.CutPrefix(ref, "refs/heads/")
			if !ok || name == "" {
				return fmt.Errorf("invalid operation branch %q in %s", ref, wt.Path)
			}
			if seen[name] {
				return nil
			}
			if _, err := r.git(ctx, "check-ref-format", ref); err != nil {
				return fmt.Errorf("invalid operation branch %q: %w", ref, err)
			}
			seen[name] = true
			protected := wt
			protected.Branch = name
			protection.branches = append(protection.branches, protected)
			return nil
		}
		if wt.Branch != "" {
			if err := protectRef("refs/heads/" + wt.Branch); err != nil {
				return protection, err
			}
		}
		admin, err := linked.gitDir(ctx)
		if err != nil {
			return protection, err
		}
		for _, dir := range []string{"rebase-merge", "rebase-apply"} {
			path := filepath.Join(admin, dir)
			if info, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return protection, err
			} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return protection, fmt.Errorf("rebase metadata is not a directory: %s", path)
			}
			data, err := operationFile(filepath.Join(path, "head-name"))
			if err != nil {
				return protection, err
			}
			// git am also uses rebase-apply, but keeps HEAD attached and has no head-name.
			if data == nil && (dir != "rebase-apply" || wt.Branch == "") {
				return protection, fmt.Errorf("missing rebase branch in %s", path)
			}
			if data != nil {
				ref := strings.TrimSuffix(string(data), "\n")
				if ref != "detached HEAD" {
					if err := protectRef(ref); err != nil {
						return protection, err
					}
				}
			}
			data, err = operationFile(filepath.Join(path, "update-refs"))
			if err != nil {
				return protection, err
			}
			if len(data) > 0 {
				lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
				if len(lines)%3 != 0 {
					return protection, fmt.Errorf("invalid rebase update-refs in %s", path)
				}
				for i := 0; i < len(lines); i += 3 {
					if !operationOID(lines[i+1]) || !operationOID(lines[i+2]) {
						return protection, fmt.Errorf("invalid rebase update-refs object in %s", path)
					}
					if err := protectRef(lines[i]); err != nil {
						return protection, err
					}
				}
			}
			for _, name := range []string{"git-rebase-todo", "done"} {
				data, err := operationFile(filepath.Join(path, name))
				if err != nil {
					return protection, err
				}
				for line := range strings.SplitSeq(string(data), "\n") {
					fields := strings.Fields(line)
					if len(fields) == 0 || (fields[0] != "update-ref" && fields[0] != "u") {
						continue
					}
					if len(fields) != 2 {
						return protection, fmt.Errorf("invalid rebase update-ref command in %s", path)
					}
					if err := protectRef(fields[1]); err != nil {
						return protection, err
					}
				}
			}
		}
		data, err := operationFile(filepath.Join(admin, "BISECT_START"))
		if err != nil {
			return protection, err
		}
		if data != nil {
			name := strings.TrimSuffix(string(data), "\n")
			if !strings.HasPrefix(name, "refs/heads/") {
				name = "refs/heads/" + name
			}
			if err := protectRef(name); err != nil {
				return protection, err
			}
		}
	}
	return protection, nil
}

func operationFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("operation metadata is not a regular file: %s", path)
	}
	return os.ReadFile(path)
}

func operationOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
