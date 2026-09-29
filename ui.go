package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

var (
	accentColor  = lipgloss.Color("#58A6FF")
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#3FB950"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#F85149"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#D29922"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B949E"))
	sectionStyle = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
)

func outputTerminal() bool { return term.IsTerminal(os.Stdout.Fd()) && os.Getenv("TERM") != "dumb" }
func interactiveTerminal() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}
func displayText(s string) string {
	if strings.ContainsFunc(s, unicode.IsControl) {
		return strconv.Quote(s)
	}
	return s
}
func colorOutput() bool {
	_, disabled := os.LookupEnv("NO_COLOR")
	return outputTerminal() && !disabled
}
func terminalWidth() int {
	if outputTerminal() {
		if width, _, err := term.GetSize(os.Stdout.Fd()); err == nil {
			return width
		}
	}
	return 0
}
func formatOutput(s string, width int, color bool) string {
	if !color {
		s = ansi.Strip(s)
	}
	if width > 0 {
		s = ansi.Wrap(s, width, "")
	}
	return s
}
func uiPrint(s string) { fmt.Fprintln(os.Stdout, formatOutput(s, terminalWidth(), colorOutput())) }
func uiBrand()         { uiPrint("\n" + sectionStyle.Render("  git tidy") + dimStyle.Render(" by kp")) }
func uiProgress(current, total int, name string) {
	uiSection(fmt.Sprintf("[%d/%d] %s", current, total, name))
}
func uiSection(s string)   { uiPrint("\n" + sectionStyle.Render("  "+displayText(s))) }
func uiWarn(s string)      { uiPrint("  " + warnStyle.Render("!") + " " + displayText(s)) }
func uiItem(s string)      { uiPrint("\n  ▸ " + displayText(s)) }
func uiDim(s string)       { uiPrint("    " + dimStyle.Render(displayText(s))) }
func uiKeep(reason string) { uiAction("KEPT", reason) }
func uiAction(action, detail string) {
	style := okStyle
	switch action {
	case "KEPT":
		style = warnStyle
	case "ARCHIVED", "UNCHANGED", "WOULD ARCHIVE", "WOULD REMOVE", "WOULD DELETE":
		style = sectionStyle
	}
	uiPrint("    " + style.Bold(true).Render("↳ "+action) + " · " + displayText(detail))
}
func uiErr(s string) {
	prefix := "  ✗ "
	_, disabled := os.LookupEnv("NO_COLOR")
	if term.IsTerminal(os.Stderr.Fd()) && os.Getenv("TERM") != "dumb" && !disabled {
		prefix = "  " + errStyle.Render("✗") + " "
	}
	for line := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		fmt.Fprintln(os.Stderr, prefix+displayText(line))
		prefix = "    "
	}
}
func shortOID(oid string) string { return oid[:min(len(oid), 12)] }
func uiBranch(b Branch)          { uiItem("Branch: " + b.Name + " · " + shortOID(b.OID)) }
func uiWorktree(base string, wt Worktree) {
	path := wt.Path
	if relative, err := filepath.Rel(base, path); err == nil {
		path = relative
	}
	branch := wt.Branch
	if branch == "" {
		branch = "detached HEAD"
	}
	uiItem("Worktree: " + path + " · " + branch + " · " + shortOID(wt.OID))
}
func uiPR(pr PR) {
	state := strings.ToLower(pr.State)
	style := dimStyle
	switch state {
	case "merged":
		style = okStyle
	case "open":
		style = warnStyle
	case "closed":
		style = errStyle
	}
	uiPrint("    " + style.Render(displayText(state)) + dimStyle.Render(fmt.Sprintf(" · PR #%d · %s", pr.Number, displayText(pr.Title))))
	if pr.URL != "" {
		uiDim(pr.URL)
	}
}
func summaryCounts(r repoResult) string {
	if r.DryRun {
		return fmt.Sprintf("worktrees %d planned / %d kept · branches %d planned / %d kept", r.WorktreesPlanned, r.WorktreesSkipped, r.BranchesPlanned, r.BranchesSkipped)
	}
	return fmt.Sprintf("worktrees %d removed / %d kept · branches %d deleted / %d kept", r.WorktreesRemoved, r.WorktreesSkipped, r.BranchesDeleted, r.BranchesSkipped)
}
func summaryStatus(r repoResult) string {
	if r.Unchanged {
		return "unchanged"
	}
	status := "done"
	if r.DryRun {
		status = "dry run"
	}
	if len(r.Errors) > 0 {
		status = "failed"
	}
	if r.Canceled {
		status = "canceled"
	}
	return status
}
func uiSummary(results []repoResult) {
	uiSection("Summary")
	var total repoResult
	var failed, canceled int
	for _, r := range results {
		status := summaryStatus(r)
		style := okStyle
		switch status {
		case "failed":
			style = errStyle
		case "canceled":
			style = warnStyle
		case "dry run", "unchanged":
			style = dimStyle
		}
		uiPrint(fmt.Sprintf("  %s · %s · %s", sectionStyle.Render(displayText(r.Name)), style.Render(status), r.Duration.Round(time.Millisecond)))
		if !r.Unchanged {
			uiDim(summaryCounts(r))
		}
		if len(r.Errors) > 0 && !r.Canceled {
			failed++
		}
		if r.Canceled {
			canceled++
		}
		for _, err := range r.Errors {
			for line := range strings.SplitSeq(strings.TrimSpace(err), "\n") {
				uiDim(line)
			}
		}
		total.WorktreesRemoved += r.WorktreesRemoved
		total.WorktreesSkipped += r.WorktreesSkipped
		total.WorktreesPlanned += r.WorktreesPlanned
		total.BranchesDeleted += r.BranchesDeleted
		total.BranchesSkipped += r.BranchesSkipped
		total.BranchesPlanned += r.BranchesPlanned
		total.Duration += r.Duration
		total.DryRun = total.DryRun || r.DryRun
	}
	if len(results) > 1 {
		uiPrint(fmt.Sprintf("\n  Total · %d repositories · %s cleanup time", len(results), total.Duration.Round(time.Millisecond)))
		uiDim(summaryCounts(total))
	}
	if failed > 0 || canceled > 0 {
		uiDim(fmt.Sprintf("%d failed · %d canceled · partial progress shown above", failed, canceled))
	}
}
func uiSpinner(text string) func() {
	if !outputTerminal() {
		return func() {}
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Printf("\r  %s %s...", frames[i%len(frames)], text)
			}
		}
	}()
	return sync.OnceFunc(func() { close(done); <-exited; fmt.Print("\r\033[K") })
}
