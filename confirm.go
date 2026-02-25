package main

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// ErrUserAborted is returned when the user presses Ctrl+C during a prompt.
var ErrUserAborted = errors.New("user aborted")

type confirmModel struct {
	title    string
	value    bool
	done     bool
	aborted  bool
}

func (m confirmModel) Init() tea.Cmd {
	return nil
}

func (m confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y":
			m.value = true
			m.done = true
			return m, tea.Quit
		case "n", "N":
			m.value = false
			m.done = true
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		case "ctrl+c":
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

	return tea.NewView(fmt.Sprintf("  %s %s / %s\n", warnStyle.Render("?")+" "+m.title, yes, no))
}

// confirm shows an interactive yes/no prompt and returns the user's choice.
// It returns ErrUserAborted if the user presses Ctrl+C.
func confirm(title string, defaultValue bool) (bool, error) {
	m := confirmModel{
		title: title,
		value: defaultValue,
	}

	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return false, err
	}

	final := result.(confirmModel)
	if final.aborted {
		return false, ErrUserAborted
	}

	return final.value, nil
}
