package main

import "testing"

func TestQuoteAppleScript(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`plain`, `"plain"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		// A PR title is interpolated straight into an osascript string, so both
		// escapes have to survive together or the notification silently fails.
		{`a "b" \ c`, `"a \"b\" \\ c"`},
	}
	for _, tt := range tests {
		if got := quoteAppleScript(tt.in); got != tt.want {
			t.Errorf("quoteAppleScript(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
