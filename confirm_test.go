package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestConfirmChoiceAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		key                     tea.KeyPressMsg
		initial, value, aborted bool
	}{
		{"yes", tea.KeyPressMsg{Code: 'y', Text: "y"}, false, true, false},
		{"no", tea.KeyPressMsg{Code: 'n', Text: "n"}, true, false, false},
		{"escape declines", tea.KeyPressMsg{Code: tea.KeyEscape}, true, false, false},
		{"enter accepts selection", tea.KeyPressMsg{Code: tea.KeyEnter}, true, true, false},
		{"quit cancels", tea.KeyPressMsg{Code: 'q', Text: "q"}, false, false, true},
		{"interrupt cancels", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated, cmd := (confirmModel{value: tc.initial}).Update(tc.key)
			got := updated.(confirmModel)
			if cmd == nil || got.value != tc.value || got.aborted != tc.aborted || (!got.done && !got.aborted) {
				t.Fatalf("unexpected confirmation result: %+v", got)
			}
		})
	}
}

func TestConfirmPlainSafeView(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	got := (confirmModel{title: "Delete branch\x1b[2J?", width: 40}).View().Content
	if strings.Contains(got, "\x1b") {
		t.Fatalf("unsafe prompt or unexpected color: %q", got)
	}
	for _, want := range []string{"esc: no", "cancel"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in prompt: %q", want, got)
		}
	}
}
