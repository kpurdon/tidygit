package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type submoduleRecovery struct {
	admin         string
	modules       map[string]string // Worktree-relative child path to owned Git directory.
	hasSubmodules bool
}

// submoduleArchivePolicy recognizes only submodules whose complete Git storage
// belongs to this linked worktree. Other nested repositories remain protected.
func (r repository) submoduleArchivePolicy(ctx context.Context, wt Worktree) (bool, error) {
	plan, err := r.submoduleRecoveryPlan(ctx, wt)
	return plan.hasSubmodules, err
}

func (r repository) ownedWorktreeAdmin(ctx context.Context, wt Worktree) (string, error) {
	linked := r
	linked.Dir = wt.Path
	admin, err := linked.gitDir(ctx)
	if err != nil {
		return "", err
	}
	expectedParent := filepath.Join(canonicalPath(r.CommonDir), "worktrees")
	if filepath.Dir(admin) != expectedParent || canonicalPath(expectedParent) != expectedParent {
		return "", errors.New("worktree Git metadata is not an owned linked-worktree directory")
	}
	info, err := os.Lstat(admin)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("worktree Git metadata is not a real directory")
	}
	common, err := os.ReadFile(filepath.Join(admin, "commondir"))
	if err != nil {
		return "", err
	}
	commonPath := strings.TrimRight(string(common), "\r\n")
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(admin, commonPath)
	}
	if canonicalPath(commonPath) != canonicalPath(r.CommonDir) {
		return "", errors.New("worktree common directory changed")
	}
	backref, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return "", err
	}
	if canonicalPath(strings.TrimRight(string(backref), "\r\n")) != filepath.Join(canonicalPath(wt.Path), ".git") {
		return "", errors.New("worktree Git metadata points at another checkout")
	}
	return admin, nil
}

func (r repository) submoduleRecoveryPlan(ctx context.Context, wt Worktree) (submoduleRecovery, error) {
	plan := submoduleRecovery{modules: map[string]string{}}
	root := canonicalPath(wt.Path)
	var adminErr error
	plan.admin, adminErr = r.ownedWorktreeAdmin(ctx, wt)
	if adminErr != nil {
		return plan, adminErr
	}
	if entries, err := os.ReadDir(filepath.Join(plan.admin, "modules")); err == nil {
		plan.hasSubmodules = len(entries) > 0
	} else if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	allowed := map[string]bool{filepath.Join(root, ".git"): true}
	var inspect func(string) error
	inspect = func(dir string) error {
		child := r
		child.Dir = dir
		entries, err := child.git(ctx, "ls-files", "--stage", "-z")
		if err != nil {
			return err
		}
		for entry := range strings.SplitSeq(entries, "\x00") {
			if !strings.HasPrefix(entry, "160000 ") {
				continue
			}
			plan.hasSubmodules = true
			_, name, ok := strings.Cut(entry, "\t")
			if !ok {
				return errors.New("invalid submodule index entry")
			}
			name = filepath.FromSlash(name)
			if err := validArchivePath(name); err != nil {
				return err
			}
			path := filepath.Join(dir, name)
			if canonicalPath(path) != path {
				return fmt.Errorf("submodule %q has a symlinked path", path)
			}
			marker := filepath.Join(path, ".git")
			info, err := os.Lstat(marker)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("submodule %q has unsupported Git metadata", path)
			}
			if plan.admin == "" {
				plan.admin, err = r.ownedWorktreeAdmin(ctx, wt)
				if err != nil {
					return err
				}
			}
			sub := r
			sub.Dir = path
			gitDir, err := sub.gitDir(ctx)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(filepath.Join(plan.admin, "modules"), gitDir)
			if err != nil || !filepath.IsLocal(relative) || relative == "." {
				return fmt.Errorf("submodule %q stores Git data outside this worktree's owned modules", path)
			}
			common, err := sub.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if err != nil {
				return err
			}
			if canonicalPath(strings.TrimSuffix(common, "\n")) != gitDir {
				return fmt.Errorf("submodule %q shares external Git data", path)
			}
			if err := sub.checkSubmoduleStorage(ctx, gitDir); err != nil {
				return fmt.Errorf("submodule %q: %w", path, err)
			}
			if operation, err := sub.inProgress(ctx); err != nil {
				return err
			} else if operation != "" {
				return fmt.Errorf("submodule %q has %s in progress", path, operation)
			}
			relative, err = filepath.Rel(root, path)
			if err != nil || !filepath.IsLocal(relative) {
				return errors.New("submodule path escapes worktree")
			}
			plan.modules[relative] = gitDir
			allowed[marker] = true
			if err := inspect(path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := inspect(root); err != nil {
		return plan, err
	}
	if plan.hasSubmodules && plan.admin == "" {
		var err error
		plan.admin, err = r.ownedWorktreeAdmin(ctx, wt)
		if err != nil {
			return plan, err
		}
	}
	if plan.hasSubmodules {
		if err := r.checkOwnedModuleTree(ctx, filepath.Join(plan.admin, "modules"), root); err != nil {
			return plan, err
		}
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if strings.EqualFold(entry.Name(), ".git") {
			if !allowed[path] {
				return fmt.Errorf("nested Git repository %q requires separate recovery archival", filepath.Dir(path))
			}
			if entry.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	return plan, err
}

func (r repository) checkSubmoduleStorage(ctx context.Context, gitDir string) error {
	if os.Getenv("GIT_ALTERNATE_OBJECT_DIRECTORIES") != "" || os.Getenv("GIT_OBJECT_DIRECTORY") != "" {
		return errors.New("external Git object storage is configured")
	}
	for _, name := range []string{"objects/info/alternates", "objects/info/http-alternates", "shallow"} {
		info, err := os.Stat(filepath.Join(gitDir, filepath.FromSlash(name)))
		if err == nil && info.Size() > 0 {
			return fmt.Errorf("unsupported external or shallow storage: %s", name)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if entries, err := os.ReadDir(filepath.Join(gitDir, "worktrees")); err == nil && len(entries) > 0 {
		return errors.New("submodule has dependent linked worktrees")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	config, err := r.git(ctx, "config", "--file", filepath.Join(gitDir, "config"), "--get-regexp", `^(extensions\.partialclone|remote\..*\.promisor|include\.path|includeif\..*\.path)$`)
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 {
			return err
		}
	} else if strings.TrimSpace(config) != "" {
		return errors.New("partial-clone/promisor storage requires separate archival")
	}
	return filepath.WalkDir(gitDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git metadata contains symlink %q", path)
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			return fmt.Errorf("Git metadata is locked: %q", path)
		}
		return nil
	})
}

// Deinitialized children can retain Git storage without a .git pointer in the
// checkout. Validate those remnants too before moving the whole admin tree.
func (r repository) checkOwnedModuleTree(ctx context.Context, root, worktree string) error {
	if os.Getenv("GIT_ALTERNATE_OBJECT_DIRECTORIES") != "" || os.Getenv("GIT_OBJECT_DIRECTORY") != "" {
		return errors.New("external Git object storage is configured")
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("owned submodule metadata contains symlink %q", path)
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".promisor") {
			return fmt.Errorf("owned submodule metadata is locked or partial: %q", path)
		}
		if entry.IsDir() && name == "worktrees" {
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(entries) > 0 {
				return fmt.Errorf("submodule has dependent linked worktrees in %q", path)
			}
		}
		if name == "alternates" || name == "http-alternates" || name == "shallow" || name == "commondir" {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > 0 {
				return fmt.Errorf("owned submodule metadata depends on external or shallow storage: %q", path)
			}
		}
		if name == "config" && !entry.IsDir() {
			configured, configErr := r.git(ctx, "config", "--file", path, "--get", "core.worktree")
			if configErr != nil {
				return fmt.Errorf("cannot verify owned submodule checkout for %q: %w", path, configErr)
			}
			target := strings.TrimSuffix(configured, "\n")
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			rel, relErr := filepath.Rel(worktree, canonicalPath(target))
			if relErr != nil || !filepath.IsLocal(rel) || rel == "." {
				return fmt.Errorf("owned submodule metadata points outside retiring worktree: %q", path)
			}
			config, err := r.git(ctx, "config", "--file", path, "--get-regexp", `^(extensions\.partialclone|remote\..*\.promisor|include\.path|includeif\..*\.path)$`)
			if err != nil {
				if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 {
					return err
				}
			} else if strings.TrimSpace(config) != "" {
				return fmt.Errorf("owned submodule metadata uses promisor storage: %q", path)
			}
		}
		return nil
	})
}
