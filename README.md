# tidygit

Tidy Git repositories, local branches, and worktrees with a styled terminal UI and plain logs when output is redirected.

## Install

Requires Git and Go 1.27.1 or later to build. Install [GitHub CLI](https://cli.github.com/) and authenticate with `gh auth login` for PR information and automatic cleanup.

```sh
go install github.com/kpurdon/tidygit@latest
```

Or build from source with `go build -o tidygit .`. CI tests Linux and macOS with real Git repositories; Windows is cross-compiled but not runtime-tested. Version tags produce Linux, macOS, and Windows archives for amd64 and arm64, with SHA-256 checksums.

## Usage

```sh
tidygit                         # Tidy the current repository
tidygit all [dir]               # Tidy repositories in a directory
tidygit --auto                  # Clean up completed work and archive stale work
tidygit --auto all [dir]
tidygit --dry-run               # Preview without changing repository state
tidygit --force                 # Recheck even when local state is unchanged
tidygit --stale-days 7          # Override the default 30-day inactivity threshold
tidygit --stale-days 0          # Disable cleanup based on age
tidygit --rebase                # Opt in to rebasing the default branch
tidygit --help
tidygit --version
```

The default branch updates with a fast-forward-only merge after fetching. Interactive removal prompts default to **No**. Automatic removal accepts merged PRs and PRs closed without merging, provided the PR head repository and commit match the local branch. PR lookup checks the local branch name before its tracked parent, and can resolve renamed branches by commit or conventional `pr123`/`pr-123` names. Detached worktrees can be removed when their tip is already on the default branch or matches a terminal PR. Squash-equivalent snapshots require a verified merged PR and a complete recovery archive before removal. Open PRs remain protected.

Worktrees and standalone branches with no open PR become eligible after **30 days without activity**, configurable with `--stale-days N`; `0` disables age-based cleanup. Activity includes commits, checkout/ref updates, staged edits, and file/directory modification times, including ignored files. A recently created worktree at an old commit is still recent. Stale worktrees are moved whole into a private archive with a self-contained Git bundle and staging snapshot; stale standalone branches are bundled before deletion. Tracked edits, untracked files, and ignored files remain recoverable. Submodules, nested repositories, incomplete history, and archival failures prevent automatic stale removal.

For clean completed worktrees, ignored files are archived before removal. Archives live under `${XDG_STATE_HOME:-~/.local/state}/tidygit/archives`, include recovery instructions, and are never automatically expired. Their locations appear during cleanup, not in the summary. Worktree archival is supported on macOS and Linux and requires the archive to be on the same filesystem. A failure retains original files or reports their recoverable archive location. The primary checkout and its ignored files, the current checkout, default branch, and locked worktrees are protected. Cleanup pauses while a worktree has an active Git operation such as a rebase or bisect. Rebasing is skipped when local files, including ignored files, are present. Automatic cleanup fails when required GitHub evidence is unavailable.

To reserve a branch and its worktree for manual cleanup, add its exact name to the repository configuration: `git config --local --add tidygit.keep my-branch`. Repeat for additional branches. Remove an exemption with `git config --local --fixed-value --unset-all tidygit.keep my-branch`. Git's `worktree lock` also protects a worktree.

Successful runs record local state under `${XDG_STATE_HOME:-~/.local/state}/tidygit/runs`. Repeat runs skip repositories whose refs, worktrees, staging, configuration, file metadata, and stale eligibility are unchanged. Crossing the inactivity threshold triggers a fresh run without `--force`. There is no PR cache or remote check for skipped repositories: use `--force` to notice PRs merged or closed only on GitHub. File-content changes preserving both size and modification time also require `--force`. Force bypasses the marker, never deletion safeguards. Dry runs always inspect repositories and never write markers or archives.

Every run ends with a summary of removals, retained work, and failures. Output keeps terminal scrollback, wraps to your terminal width, and honors `NO_COLOR`. In prompts, use `y`/`n`, arrows and Enter, Esc to keep the item, or `q`/Ctrl+C to cancel the run.

`--dry-run` performs read-only discovery: it does not reset, switch, fetch, update, prune, or delete. Failures return a nonzero exit status; interruption returns 130.

| Option | Default | Purpose |
|--------|---------|---------|
| `--pr-limit N` | `100` | Maximum PRs per batch or targeted lookup |
| `--stale-days N` | `30` | Archive inactive work before removal; `0` disables age-based cleanup |
| `--exclude-pr-filtering` | Off | Remove the default `@me` author filter from the initial batch; targeted fallback lookups include other authors regardless |
| `--timeout duration` | `30s` | Timeout for each external command |
| `--jobs N` | `4` | Concurrent read-only repository discovery and PR lookups |

## Shell aliases

```zsh
alias git_clean='tidygit'
alias git_clean_all='tidygit all'
alias git_clean_auto='tidygit --auto all'
```

[MIT license](LICENSE).
