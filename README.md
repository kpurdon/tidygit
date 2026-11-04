# tidygit

Tidy Git repositories, local branches, and worktrees with a styled terminal UI and plain logs when output is redirected.

## Install

Requires Git and Go 1.27.2 or later to build. Install [GitHub CLI](https://cli.github.com/) and authenticate with `gh auth login` for PR information and automatic cleanup.

```sh
go install github.com/kpurdon/tidygit@latest
```

Or build from source with `go build -o tidygit .`. CI tests Linux and macOS with real Git repositories; Windows is cross-compiled but not runtime-tested. Version tags produce Linux, macOS, and Windows archives for amd64 and arm64, with SHA-256 checksums.

## Usage

```sh
tidygit                         # Tidy the current repository
tidygit all [dir]               # Tidy repositories in a directory
tidygit --auto                  # Remove completed worktrees, including ignored files
tidygit --auto --archive        # Also allow recovery archives before removal
tidygit --auto all [dir]
tidygit --dry-run               # Preview without changing repository state
tidygit --force                 # Recheck even when local state is unchanged
tidygit --archive --stale-days 7 # Archive work inactive for at least 7 days
tidygit --stale-days 0          # Disable cleanup based on age
tidygit --rebase                # Opt in to rebasing the default branch
tidygit --help
tidygit --version
```

The default branch updates with a fast-forward-only merge after fetching. Initialized submodules follow the recorded revisions after branch switches and updates; collisions with ignored files stop the update rather than overwrite those files. Interactive removal prompts default to **No**. Automatic removal accepts merged PRs and PRs closed without merging, provided the PR head repository and commit match the local branch. PR lookup checks the local branch name before its tracked parent, and can resolve renamed branches by commit or conventional `pr123`/`pr-123` names. Detached worktrees can be removed when their tip is already on the default branch or matches a terminal PR. Squash-equivalent snapshots require a verified merged PR and `--archive` to preserve a complete recovery archive before removal. Named branches without a matching PR can also be removed when their exact tip is contained in the fetched default branch. Open PRs remain protected.

With `--archive`, worktrees and standalone branches with no open PR become eligible after **7 days without activity**, configurable with `--stale-days N`; `0` disables age-based cleanup. Activity includes commits, checkout/ref updates, staged edits, and file/directory modification times, including ignored files. Retention messages report the last known activity and configured threshold; unknown activity age remains protected. A recently created worktree at an old commit is still recent. Reflog maintenance and ref packing do not reset the inactivity clock; recorded reflog events do. Stale worktrees are moved whole into a private archive with a self-contained Git bundle and staging snapshot; stale standalone branches are bundled before deletion. Tracked edits, untracked files, and ignored files remain recoverable. Submodules, nested repositories, incomplete history, and archival failures prevent automatic stale removal.

Archiving is disabled by default. Verified-completed worktrees with clean tracked and untracked source are removed together with all ignored files, including dependency environments, caches, reports, and local configuration. Use `--archive` to retain recovery copies of those files instead. Unfinished source and Git history that cannot be safely recovered stay in place with a `KEPT` explanation. Archives live under `${XDG_STATE_HOME:-~/.local/state}/tidygit/archives`, include recovery instructions, and are never automatically expired. Their locations appear during cleanup, not in the summary. Worktree archival is supported on macOS and Linux and requires the archive to be on the same filesystem. A failure retains original files or reports their recoverable archive location. The primary checkout and its ignored files, the current checkout, default branch, and locked worktrees are protected. An active Git operation protects its worktree and affected branches, including branches scheduled for rebase updates; unrelated completed work can still be removed. Cleanup invoked from a checkout with an active operation stops before updating it. Rebasing is skipped when local files, including ignored files, are present. Automatic cleanup fails when required GitHub evidence is unavailable.

Clean, verified-completed worktrees with initialized submodules can be removed without archives when stored child objects are recoverable from currently advertised origin branches or tags, or belong exclusively to cached history of deleted origin branches. Cached history still used by local refs or checkout history remains protected. Unique local refs, stashes, changes, unclassified objects, unreadable history, and unverifiable remote state prevent removal. With `--archive`, the entire checkout and its owned submodule Git storage are preserved before removal instead. Shared or external submodule storage and unsupported nested repositories are retained with a `KEPT` explanation. Submodule worktrees with local changes or without verified completion remain for manual cleanup.

To reserve a branch and its worktree for manual cleanup, add its exact name to the repository configuration: `git config --local --add tidygit.keep my-branch`. Repeat for additional branches. Remove an exemption with `git config --local --fixed-value --unset-all tidygit.keep my-branch`. Git's `worktree lock` also protects a worktree.

Packed branches without surviving reflog events or an individual update timestamp have unknown activity age and remain for manual cleanup.

Successful runs record local state under `${XDG_STATE_HOME:-~/.local/state}/tidygit/runs`. Repeat runs skip repositories whose refs, worktrees, staging, configuration, file metadata, and stale eligibility are unchanged. Crossing the inactivity threshold triggers a fresh run without `--force`. There is no PR cache or remote check for skipped repositories: use `--force` to notice PRs merged or closed only on GitHub. File-content changes preserving both size and modification time also require `--force`. Force bypasses the marker, never deletion safeguards. Dry runs always inspect repositories and never write markers or archives.

Every run ends with a summary of removals, retained work, and failures. Output keeps terminal scrollback, wraps to your terminal width, and honors `NO_COLOR`. In prompts, use `y`/`n`, arrows and Enter, Esc to keep the item, or `q`/Ctrl+C to cancel the run.

`--dry-run` performs read-only discovery: it does not reset, switch, fetch, update, prune, or delete. Failures return a nonzero exit status; interruption returns 130.

| Option | Default | Purpose |
|--------|---------|---------|
| `--pr-limit N` | `100` | Maximum PRs per batch or targeted lookup |
| `--archive` | Off | Allow recovery archives before removing work that needs preservation |
| `--stale-days N` | `7` | With `--archive`, archive inactive work before removal; `0` disables age-based cleanup |
| `--exclude-pr-filtering` | Off | Remove the default `@me` author filter from the initial batch; targeted fallback lookups include other authors regardless |
| `--timeout duration` | `30s` | Timeout for each Git/GitHub command |
| `--agent-timeout duration` | `5m` | Timeout for each agent repair attempt |
| `--jobs N` | `4` | Concurrent read-only repository discovery, PR lookups, and marker scans |

## Agent repair

Install and authenticate Codex or Claude Code, select it with Git configuration, and opt in to repair for a run:

```sh
git config --global tidygit.agent codex  # or claude
tidygit --auto --fix
```

`--fix` allows one agent repair attempt per repository when default-branch detection, fetching, or the initial PR lookup fails. The agent can inspect the repository and use shell and file tools to repair local Git metadata, including problems such as case-colliding remote-tracking refs. It is instructed to preserve local work and configuration, avoid history rewrites and remote writes, and report unresolved problems. Before resuming cleanup, tidygit checks repository identity, local refs, worktrees, staging, file metadata, and configuration, then reruns normal cleanup checks. `--dry-run` never invokes the agent.

The selected provider receives diagnostics and can read repository content through its tools. Codex uses automatic approval review with a workspace-write sandbox and explicit access to the repository and its Git directories; Claude uses its auto permission mode. CLI authentication is reused, while hooks and unrelated MCP integrations are disabled. Agent permissions and organization policy govern execution, rather than a fixed tidygit command allowlist. Use a current CLI and account supporting these modes; refusal or unsupported flags leave the failure unresolved.

Repair transcripts are saved under `${XDG_STATE_HOME:-~/.local/state}/tidygit/repairs`; this location must be outside the repository's worktrees. Use `--agent-timeout 10m` to override the five-minute agent timeout independently of Git/GitHub command timeouts. Repairs happen in place: a failed or interrupted attempt can leave changes, which tidygit does not automatically roll back.

## Shell aliases

```zsh
alias git_clean='tidygit'
alias git_clean_all='tidygit all'
alias git_clean_auto='tidygit --auto all'
```

[MIT license](LICENSE).
