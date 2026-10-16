package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	state, err := localSnapshotAt(ctx, r, opts, now)
	if err != nil {
		return "", err
	}
	return state.Fingerprint(), nil
}

// Bump when cleanup eligibility or fingerprint semantics change, not for UI builds.
const cacheSemanticsVersion = 4

type stateRef struct{ OID, Symref string }
type stateConfig struct {
	Origin, Key, Value string
	Root               bool
}
type localState struct {
	Branches   []Branch
	Worktrees  []Worktree
	base       string
	refs       map[string]stateRef
	config     []stateConfig
	trees      map[string]string
	branchAges map[string]bool
}

func localSnapshot(ctx context.Context, r repository, opts options) (*localState, error) {
	return localSnapshotAt(ctx, r, opts, time.Now())
}

func localSnapshotAt(ctx context.Context, r repository, opts options, now time.Time) (*localState, error) {
	state := &localState{refs: map[string]stateRef{}, trees: map[string]string{}, branchAges: map[string]bool{}}
	h := sha256.New()
	fingerprintField(h, fmt.Sprintf("v%d:%s:%s:%t:%t:%d:%s:%d", cacheSemanticsVersion, canonicalPath(r.CommonDir), canonicalPath(r.ProtectedPath), opts.Auto, opts.Rebase, opts.PRLimit, opts.PRAuthor, opts.StaleDays))
	raw, err := r.git(ctx, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)")
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 3 {
			return nil, errors.New("invalid ref snapshot")
		}
		state.refs[fields[0]] = stateRef{fields[1], fields[2]}
	}
	raw, err = r.git(ctx, "config", "--null", "--list", "--show-origin")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	if len(fields)%2 != 0 {
		return nil, errors.New("invalid config snapshot")
	}
	for i := 0; i < len(fields); i += 2 {
		key, value, _ := strings.Cut(fields[i+1], "\n")
		entry := stateConfig{Origin: fields[i], Key: key, Value: value}
		if path, ok := strings.CutPrefix(entry.Origin, "file:"); ok {
			if !filepath.IsAbs(path) {
				path = filepath.Join(r.Dir, path)
			}
			entry.Root = canonicalPath(path) == canonicalPath(filepath.Join(r.CommonDir, "config"))
		}
		state.config = append(state.config, entry)
	}
	// Repository-local ignore and attribute rules can change classification without
	// changing refs or the worktree files themselves.
	for _, name := range []string{"info/exclude", "info/attributes"} {
		path := filepath.Join(r.CommonDir, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		fingerprintField(h, name)
		fingerprintField(h, string(data))
	}
	for _, rule := range []struct{ key, fallback string }{{"core.excludesFile", "ignore"}, {"core.attributesFile", "attributes"}} {
		out, err := r.git(ctx, "config", "--null", "--path", "--default", "", "--get", rule.key)
		if err != nil {
			return nil, err
		}
		path := strings.TrimSuffix(out, "\x00")
		if path == "" {
			base := os.Getenv("XDG_CONFIG_HOME")
			if base == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return nil, err
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
			return nil, err
		}
		fingerprintField(h, path)
		fingerprintField(h, string(data))
	}
	worktrees, err := r.worktrees(ctx)
	if err != nil {
		return nil, err
	}
	state.base = fmt.Sprintf("%x", h.Sum(nil))
	state.Worktrees = worktrees
	checkedOut := map[string]bool{}
	for _, wt := range worktrees {
		checkedOut[wt.Branch] = true
		h := sha256.New()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fingerprintField(h, fmt.Sprintf("%q:%q:%q:%t:%t", wt.Path, wt.Branch, wt.OID, wt.Locked, wt.Prunable))
		wr := r
		wr.Dir = wt.Path
		admin, err := wr.gitDir(ctx)
		if err != nil {
			return nil, err
		}
		// Index entries, not index timestamps/stat-cache extensions, describe staging.
		index, err := wr.git(ctx, "ls-files", "--stage", "-v", "-z")
		if err != nil {
			return nil, err
		}
		fingerprintField(h, index)
		for _, name := range []string{"HEAD", "config.worktree", "rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer", "BISECT_START"} {
			path := filepath.Join(admin, name)
			if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				fingerprintField(h, name+":absent")
				continue
			} else if err != nil {
				return nil, err
			}
			fingerprintField(h, name)
			if err := fingerprintTree(ctx, h, path, true); err != nil {
				return nil, err
			}
		}
		latest, err := fingerprintWorktree(ctx, h, wt.Path)
		if err != nil {
			return nil, err
		}
		if opts.StaleDays > 0 {
			latest, err = worktreeMetadataActivity(ctx, wr, latest)
			if err != nil {
				return nil, err
			}
		}
		fingerprintField(h, fmt.Sprint(activityIsStale(latest, opts.StaleDays, now)))
		state.trees[canonicalPath(wt.Path)] = fmt.Sprintf("%x", h.Sum(nil))
	}
	state.Branches, err = r.branches(ctx)
	if err != nil {
		return nil, err
	}
	for _, branch := range state.Branches {
		ref, ok := state.refs["refs/heads/"+branch.Name]
		if !ok || ref.OID != branch.OID {
			return nil, errors.New("branches changed during snapshot")
		}
		if opts.StaleDays > 0 && !checkedOut[branch.Name] {
			stale, err := staleBranch(ctx, r, branch, opts.StaleDays, now)
			if err != nil {
				return nil, err
			}
			state.branchAges[branch.Name] = stale
		}
	}
	return state, nil
}

func fingerprintField(h hash.Hash, value string) { fmt.Fprintf(h, "%d:%s", len(value), value) }

func fingerprintTree(ctx context.Context, h hash.Hash, root string, contents bool) error {
	_, err := fingerprintTreeActivity(ctx, h, root, contents)
	return err
}
func fingerprintWorktree(ctx context.Context, h hash.Hash, root string) (time.Time, error) {
	return fingerprintTreeActivity(ctx, h, root, false)
}
func fingerprintTreeActivity(ctx context.Context, h hash.Hash, root string, contents bool) (time.Time, error) {
	var latest time.Time
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if !contents && entry.Name() == ".git" {
			if !entry.IsDir() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.ModTime().After(latest) {
					latest = info.ModTime()
				}
			}
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
		if info.ModTime().After(latest) {
			latest = info.ModTime()
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
	return latest, err
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

func (s *localState) Fingerprint() string {
	h := sha256.New()
	fingerprintField(h, s.base)
	for _, name := range slices.Sorted(maps.Keys(s.refs)) {
		ref := s.refs[name]
		fingerprintField(h, name)
		fingerprintField(h, ref.OID)
		fingerprintField(h, ref.Symref)
	}
	for _, entry := range s.config {
		fingerprintField(h, entry.Origin)
		fingerprintField(h, entry.Key)
		fingerprintField(h, entry.Value)
	}
	for _, path := range slices.Sorted(maps.Keys(s.trees)) {
		fingerprintField(h, path)
		fingerprintField(h, s.trees[path])
	}
	checked := map[string]bool{}
	for _, wt := range s.Worktrees {
		checked[wt.Branch] = true
	}
	for _, name := range slices.Sorted(maps.Keys(s.branchAges)) {
		if !checked[name] {
			fingerprintField(h, name)
			fingerprintField(h, fmt.Sprint(s.branchAges[name]))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// AfterCleanup applies only verified deletions to the captured baseline. It does
// not bless unrelated changes that happened while cleanup was running.
func (s *localState) AfterCleanup(branches []Branch, worktrees []Worktree) (string, bool) {
	next := *s
	next.refs = maps.Clone(s.refs)
	next.trees = maps.Clone(s.trees)
	next.branchAges = maps.Clone(s.branchAges)
	next.Worktrees = slices.Clone(s.Worktrees)
	next.config = slices.Clone(s.config)
	for _, wt := range worktrees {
		path := canonicalPath(wt.Path)
		index := slices.IndexFunc(next.Worktrees, func(candidate Worktree) bool { return canonicalPath(candidate.Path) == path })
		if index < 0 {
			return "", false
		}
		original := next.Worktrees[index]
		if original.OID != wt.OID || original.Branch != wt.Branch {
			return "", false
		}
		delete(next.trees, path)
		next.Worktrees = slices.Delete(next.Worktrees, index, index+1)
	}
	for _, branch := range branches {
		name := "refs/heads/" + branch.Name
		ref, ok := next.refs[name]
		if !ok || ref.OID != branch.OID || ref.Symref != "" {
			return "", false
		}
		delete(next.refs, name)
		delete(next.branchAges, branch.Name)
		next.config = slices.DeleteFunc(next.config, func(entry stateConfig) bool {
			section, _, ok := strings.CutLast(entry.Key, ".")
			return ok && entry.Root && section == "branch."+branch.Name
		})
	}
	return next.Fingerprint(), true
}
