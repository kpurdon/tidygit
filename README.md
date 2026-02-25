# tidygit

> **Warning**: This project was entirely vibe coded with [Claude Code](https://claude.ai/claude-code). Use at your own risk.

A styled CLI tool for tidying up git repositories. Built with Go and [Charm](https://charm.sh) v2 libraries.

## What it does

1. Detects the default branch from `origin HEAD`
2. Checks for uncommitted changes (prompts to reset)
3. Switches to the default branch
4. Fetches all remotes with pruning
5. Pulls with rebase
6. Batch-fetches open PRs via `gh pr list` (graceful degradation if `gh` unavailable)
7. Lists worktrees and prompts for removal (shows PR info)
8. Lists branches and prompts for deletion (shows PR info)

Errors are tracked and reported but don't stop execution.

## Usage

```sh
# Tidy the current repo
tidygit

# Tidy all repos in a directory
tidygit all [dir]
```

In `all` mode, each repo is processed with a progress spinner at the top. After all repos are processed, a summary is displayed showing stats for each repo.

## Install

```sh
go install github.com/kpurdon/tidygit@latest
```

Or build from source:

```sh
go build -o tidygit ./
```

## Dependencies

| Library | Purpose |
|---------|---------|
| [lipgloss v2](https://charm.land/lipgloss/v2) | Styled text output |
| [bubbletea v2](https://charm.land/bubbletea/v2) | Interactive confirm prompts |

Optional: [gh](https://cli.github.com/) for PR information.

## Shell aliases

```zsh
alias git_clean='tidygit'
alias git_clean_all='tidygit all'
```
