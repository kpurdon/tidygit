package main

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// ErrUserAborted is returned when the user presses Ctrl+C or q during a prompt.
var ErrUserAborted = errors.New("user aborted")

type confirmModel struct {
	title   string
	value   bool
	done    bool
	aborted bool
	width   int
}

func (m confirmModel) Init() tea.Cmd {
	return nil
}

func (m confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyPressMsg:
		switch msg.String() {
		case "y", "Y":
			m.value = true
			m.done = true
			return m, tea.Quit
		case "n", "N", "esc":
			m.value = false
			m.done = true
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		case "ctrl+c", "q", "Q":
			m.aborted = true
			return m, tea.Quit
		case "left", "h":
			m.value = true
		case "right", "l":
			m.value = false
		}
	}
	return m, nil
}

func (m confirmModel) View() tea.View {
	yes := "Yes"
	no := "No"
	if m.value {
		yes = okStyle.Render("▸ Yes")
		no = dimStyle.Render("  No")
	} else {
		yes = dimStyle.Render("  Yes")
		no = errStyle.Render("▸ No")
	}

	view := fmt.Sprintf("  %s %s / %s\n", warnStyle.Render("?")+" "+displayText(m.title), yes, no)
	if !m.done && !m.aborted {
		view += dimStyle.Render("    y/n · ←/→ + enter · esc: no · q/ctrl+c: cancel") + "\n"
	}
	return tea.NewView(formatOutput(view, m.width, colorOutput()))
}

// confirm shows an interactive yes/no prompt and returns the user's choice.
// It returns ErrUserAborted if the user presses Ctrl+C or q.
func confirm(ctx context.Context, title string, defaultValue bool) (bool, error) {
	m := confirmModel{
		title: title,
		value: defaultValue,
		width: terminalWidth(),
	}

	p := tea.NewProgram(m, tea.WithContext(ctx))
	result, err := p.Run()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}

	final := result.(confirmModel)
	if final.aborted {
		return false, ErrUserAborted
	}

	return final.done && final.value, nil
}
