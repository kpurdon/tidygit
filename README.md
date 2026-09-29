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
tidygit --auto                  # Clean up verified merged or closed PR branches/worktrees
tidygit --auto all [dir]
tidygit --dry-run               # Preview without changing repository state
tidygit --force                 # Recheck even when local state is unchanged
tidygit --rebase                # Opt in to rebasing the default branch
tidygit --help
tidygit --version
```

The default branch updates with a fast-forward-only merge after fetching. Interactive removal prompts default to **No**. Automatic removal accepts merged PRs and PRs closed without merging, provided the PR head repository and commit match the local branch. Open PRs, newer local commits, and branches without a verified matching PR are preserved in automatic mode.

Tracked changes and untracked files prevent worktree removal. For a verified merged or closed PR, ignored files are moved to a private archive before removing the worktree. Each archive preserves relative paths and includes recovery instructions; its location is printed during cleanup and in the summary. Archives live under `${XDG_STATE_HOME:-~/.local/state}/tidygit/archives` and are never automatically expired. Archival is supported on macOS and Linux and requires the worktree and archive to be on the same filesystem; a failure keeps the worktree and reports any files already archived. Current, main, and locked worktrees are protected. Branch cleanup pauses while any worktree has an active Git operation such as a rebase or bisect. Clean up dirty worktrees manually before retrying. Rebasing is skipped when local files, including ignored files, are present. Without `gh`, interactive mode warns and continues without PR information; automatic mode fails when PR lookup is needed.

Successful runs record local state under `${XDG_STATE_HOME:-~/.local/state}/tidygit/runs`. Repeat runs skip repositories whose refs, worktrees, staging, configuration, and file metadata are unchanged. There is no PR cache or remote check for skipped repositories: use `--force` to notice PRs merged or closed only on GitHub. File-content changes preserving both size and modification time also require `--force`. Force bypasses the marker, never deletion safeguards. Dry runs always inspect repositories and never write markers or archives.

Every run ends with a summary of removals, retained work, and failures. Output keeps terminal scrollback, wraps to your terminal width, and honors `NO_COLOR`. In prompts, use `y`/`n`, arrows and Enter, Esc to keep the item, or `q`/Ctrl+C to cancel the run.

`--dry-run` performs read-only discovery: it does not reset, switch, fetch, update, prune, or delete. Failures return a nonzero exit status; interruption returns 130.

| Option | Default | Purpose |
|--------|---------|---------|
| `--pr-limit N` | `100` | Maximum PRs per batch or targeted lookup |
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
