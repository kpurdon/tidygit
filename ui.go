package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	// GitHub Primer dark palette
	accentColor = lipgloss.Color("#58A6FF")
	greenColor  = lipgloss.Color("#3FB950")
	redColor    = lipgloss.Color("#F85149")
	yellowColor = lipgloss.Color("#D29922")
	blueColor   = lipgloss.Color("#58A6FF")
	dimColor    = lipgloss.Color("#8B949E")
	purpleColor = lipgloss.Color("#A371F7")
	borderColor = lipgloss.Color("#30363D")

	brandStyle   = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	brandDim     = lipgloss.NewStyle().Foreground(dimColor)
	okStyle      = lipgloss.NewStyle().Foreground(greenColor)
	errStyle     = lipgloss.NewStyle().Foreground(redColor)
	warnStyle    = lipgloss.NewStyle().Foreground(yellowColor)
	sectionStyle = lipgloss.NewStyle().Foreground(blueColor).Bold(true)
	itemStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(dimColor)
	prStyle      = lipgloss.NewStyle().Foreground(yellowColor).Italic(true)
	prURLStyle   = lipgloss.NewStyle().Foreground(blueColor).Underline(true)

	summaryBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Padding(1, 2)
)

// Package-level progress spinner — stopped before any huh prompt.
var stopProgress func()

func uiBrand() {
	fmt.Println()
	fmt.Println(
		brandStyle.Render("  git tidy") +
			brandDim.Render(" by kp"),
	)
}

// uiProgress renders a static progress counter (no spinner).
func uiProgress(current, total int, repoName string) {
	counter := dimStyle.Render(fmt.Sprintf("  [%d/%d]", current, total))
	name := sectionStyle.Render(" " + repoName)
	fmt.Println(counter + name)
	fmt.Println(dimStyle.Render("  " + strings.Repeat("─", 40)))
}

// uiProgressSpinner renders a progress counter with animated spinner on row 3.
// Must be stopped with uiStopProgress() before any interactive prompt.
func uiProgressSpinner(current, total int, repoName string) {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	var once sync.Once
	done := make(chan struct{})

	// Row 3: after clear (row 1 = blank from uiBrand, row 2 = brand text, row 3 = progress)
	const row = 3

	// Print initial static line + divider
	counter := dimStyle.Render(fmt.Sprintf("  [%d/%d]", current, total))
	spinner := okStyle.Render(frames[0])
	name := sectionStyle.Render(" " + repoName)
	fmt.Println(counter + " " + spinner + name)
	fmt.Println(dimStyle.Render("  " + strings.Repeat("─", 40)))

	go func() {
		i := 1
		for {
			select {
			case <-done:
				return
			default:
				line := counter + " " + okStyle.Render(frames[i%len(frames)]) + name
				// Save cursor, move to row 3, clear line, write, restore cursor
				fmt.Printf("\033[s\033[%d;1H\033[K%s\033[u", row, line)
				i++
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()

	stopProgress = func() {
		once.Do(func() {
			close(done)
			// Replace spinner with check mark
			line := counter + " " + okStyle.Render("✓") + name
			fmt.Printf("\033[s\033[%d;1H\033[K%s\033[u", row, line)
		})
	}
}

// uiStopProgress stops the progress spinner if running.
func uiStopProgress() {
	if stopProgress != nil {
		stopProgress()
		stopProgress = nil
	}
}

func uiSection(text string) {
	fmt.Println()
	fmt.Println(sectionStyle.Render("  " + text))
	fmt.Println()
}

func uiOK(text string) {
	fmt.Println("  " + okStyle.Render("✓") + " " + text)
}

func uiErr(text string) {
	fmt.Println("  " + errStyle.Render("✗") + " " + text)
}

func uiWarn(text string) {
	fmt.Println("  " + warnStyle.Render("!") + " " + text)
}

func uiItem(text string) {
	fmt.Println("  " + itemStyle.Render("▸") + " " + text)
}

func uiDim(text string) {
	fmt.Println("  " + dimStyle.Render(text))
}

func uiPR(pr PR) {
	sep := dimStyle.Render(" · ")
	fmt.Println("    " + prStyle.Render(fmt.Sprintf("PR #%d", pr.Number)) + sep + styledPRState(pr.State) + sep + prStyle.Render(pr.Title))
	fmt.Println("    " + prURLStyle.Render(pr.URL))
}

func styledPRState(state string) string {
	s := strings.ToLower(state)
	switch s {
	case "open":
		return okStyle.Render(s)
	case "merged":
		return lipgloss.NewStyle().Foreground(purpleColor).Render(s)
	case "closed":
		return errStyle.Render(s)
	default:
		return dimStyle.Render(s)
	}
}

func uiSkipped() {
	fmt.Println("    " + dimStyle.Render("· Skipped"))
}

func uiClearScreen() {
	fmt.Print("\033[2J\033[H")
}

func uiDone() {
	fmt.Println()
	fmt.Println("  " + okStyle.Render("✓ Done"))
	fmt.Println()
}

// styledKept renders a count in green (kept/active = good).
func styledKept(n int, label string) string {
	s := fmt.Sprintf("%d", n)
	if n == 0 {
		return dimStyle.Render(s + " " + label)
	}
	return okStyle.Render(s+" "+label)
}

// styledRemoved renders a count in red (removed/deleted = destructive).
func styledRemoved(n int, label string) string {
	s := fmt.Sprintf("%d", n)
	if n == 0 {
		return dimStyle.Render(s + " " + label)
	}
	return errStyle.Render(s+" "+label)
}

func uiSummary(results []repoResult) {
	uiBrand()
	fmt.Println()

	// Aggregate totals
	var totalRepos, reposClean, reposWithErrors int
	var totalWorktrees, totalWorktreesRemoved, totalWorktreesKept int
	var totalBranches, totalBranchesDeleted, totalBranchesKept int
	var totalPRs, totalErrors int

	for _, r := range results {
		totalRepos++
		totalWorktrees += r.WorktreesTotal
		totalWorktreesRemoved += r.WorktreesRemoved
		totalWorktreesKept += r.WorktreesSkipped
		totalBranches += r.BranchesTotal
		totalBranchesDeleted += r.BranchesDeleted
		totalBranchesKept += r.BranchesSkipped
		totalPRs += r.PRsFound
		totalErrors += len(r.Errors)
		if len(r.Errors) > 0 {
			reposWithErrors++
		} else {
			reposClean++
		}
	}

	// Per-repo lines
	sep := dimStyle.Render(" · ")
	var repoLines []string
	for _, r := range results {
		icon := okStyle.Render("✓")
		if len(r.Errors) > 0 {
			icon = errStyle.Render("✗")
		}

		header := fmt.Sprintf("%s %s", icon, itemStyle.Render(r.Name))

		detail := fmt.Sprintf("    %s%s%s%s%s",
			styledRemoved(r.WorktreesRemoved, "wt removed")+sep+styledKept(r.WorktreesSkipped, "wt kept"),
			sep,
			styledRemoved(r.BranchesDeleted, "br deleted")+sep+styledKept(r.BranchesSkipped, "br kept"),
			sep,
			styledKept(r.PRsFound, "pr(s)"),
		)

		if len(r.Errors) > 0 {
			detail += sep + errStyle.Render(fmt.Sprintf("%d error(s)", len(r.Errors)))
		}

		repoLines = append(repoLines, header+"\n"+detail)
	}

	// Build box content
	content := strings.Join(repoLines, "\n")
	content += "\n\n" + dimStyle.Render(strings.Repeat("─", 44))

	// Stats table — right-justify labels so values align
	statsLabel := func(label string) string {
		return sectionStyle.Render(fmt.Sprintf("%9s", label))
	}
	content += "\n"
	content += fmt.Sprintf("  %s  %s · %s\n",
		statsLabel("Repos"),
		styledKept(reposClean, "clean"),
		styledRemoved(reposWithErrors, "with errors"),
	)
	content += fmt.Sprintf("  %s  %s · %s · %s\n",
		statsLabel("Worktrees"),
		styledKept(totalWorktrees-totalWorktreesRemoved, "active"),
		styledRemoved(totalWorktreesRemoved, "removed"),
		styledKept(totalWorktreesKept, "kept"),
	)
	content += fmt.Sprintf("  %s  %s · %s · %s\n",
		statsLabel("Branches"),
		styledKept(totalBranches-totalBranchesDeleted, "active"),
		styledRemoved(totalBranchesDeleted, "deleted"),
		styledKept(totalBranchesKept, "kept"),
	)
	content += fmt.Sprintf("  %s  %s",
		statsLabel("PRs"),
		styledKept(totalPRs, "found"),
	)
	if totalErrors > 0 {
		content += fmt.Sprintf("\n  %s  %s",
			statsLabel("Errors"),
			errStyle.Render(fmt.Sprintf("%d", totalErrors)),
		)
	}

	// Error details — wrap long messages to keep the summary box readable
	errWrapStyle := dimStyle.Width(44)
	var errLines []string
	for _, r := range results {
		for _, e := range r.Errors {
			prefix := errStyle.Render("  "+r.Name) + dimStyle.Render(": ")
			errLines = append(errLines, prefix+errWrapStyle.Render(e))
		}
	}
	if len(errLines) > 0 {
		content += "\n\n" + dimStyle.Render(strings.Repeat("─", 44))
		content += "\n" + strings.Join(errLines, "\n")
	}

	fmt.Println(summaryBoxStyle.Render(content))
	fmt.Println()
}

func uiSpinner(text string) func() {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	var once sync.Once
	done := make(chan struct{})

	go func() {
		i := 0
		for {
			select {
			case <-done:
				return
			default:
				fmt.Printf("\r  %s %s...",
					okStyle.Render(frames[i%len(frames)]),
					text,
				)
				i++
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()

	return func() {
		once.Do(func() {
			close(done)
			fmt.Print("\r\033[K")
		})
	}
}
