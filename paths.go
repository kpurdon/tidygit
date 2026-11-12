package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func stateOutsideWorktree(worktree, base string) error {
	source := canonicalPath(worktree)
	candidate, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr == nil {
			candidate = filepath.Join(append([]string{resolved}, suffix...)...)
			break
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return resolveErr
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return resolveErr
		}
		suffix = append([]string{filepath.Base(candidate)}, suffix...)
		candidate = parent
	}
	if rel, err := filepath.Rel(source, candidate); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return errors.New("state directory must be outside the worktree")
	}
	return nil
}

func validSubmodulePath(name string) error {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name || name == "." {
		return fmt.Errorf("unsafe submodule path %q", name)
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("refusing Git metadata path %q", name)
		}
	}
	return nil
}
