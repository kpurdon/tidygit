package main

import (
	"fmt"
	"os"
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
func uiPrint(s string) {
	if !outputTerminal() || os.Getenv("NO_COLOR") != "" {
		s = ansi.Strip(s)
	}
	fmt.Fprintln(os.Stdout, s)
}
func uiBrand() { uiPrint("\n" + sectionStyle.Render("  git tidy") + dimStyle.Render(" by kp")) }
func uiProgress(current, total int, name string) {
	uiPrint(fmt.Sprintf("  [%d/%d] %s", current, total, displayText(name)))
}
func uiSection(s string)   { uiPrint("\n" + sectionStyle.Render("  "+displayText(s)) + "\n") }
func uiOK(s string)        { uiPrint("  " + okStyle.Render("✓") + " " + displayText(s)) }
func uiWarn(s string)      { uiPrint("  " + warnStyle.Render("!") + " " + displayText(s)) }
func uiItem(s string)      { uiPrint("  ▸ " + displayText(s)) }
func uiDim(s string)       { uiPrint("    " + dimStyle.Render(displayText(s))) }
func uiKeep(reason string) { uiDim("Kept: " + reason) }
func uiErr(s string) {
	prefix := "  ✗ "
	if term.IsTerminal(os.Stderr.Fd()) && os.Getenv("NO_COLOR") == "" {
		prefix = "  " + errStyle.Render("✗") + " "
	}
	fmt.Fprintln(os.Stderr, prefix+displayText(s))
}
func uiPR(pr PR) {
	uiDim(fmt.Sprintf("PR #%d · %s · %s", pr.Number, strings.ToLower(pr.State), pr.Title))
	uiDim(pr.URL)
}
func uiClearScreen() {
	if outputTerminal() {
		fmt.Print("\033[2J\033[H")
	}
}
func uiDone() { uiOK("Done"); uiPrint("") }
func uiSummary(results []repoResult) {
	uiSection("Summary")
	for _, r := range results {
		uiItem(r.Name)
		uiDim(fmt.Sprintf("Worktrees: %d removed · %d kept · %d planned", r.WorktreesRemoved, r.WorktreesSkipped, r.WorktreesPlanned))
		uiDim(fmt.Sprintf("Branches: %d deleted · %d kept · %d planned", r.BranchesDeleted, r.BranchesSkipped, r.BranchesPlanned))
		if len(r.Errors) > 0 {
			uiDim(fmt.Sprintf("%d error(s)", len(r.Errors)))
			for _, err := range r.Errors {
				uiDim(err)
			}
		}
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
