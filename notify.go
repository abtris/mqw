package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// notifier raises a user-visible notification. Swapped out in tests.
var notifier = notify

// notify raises a desktop notification and rings the terminal bell. Failures are
// deliberately ignored: a missing notifier must not take down the watcher.
func notify(title, message string) {
	os.Stderr.WriteString("\a")

	if runtime.GOOS != "darwin" {
		return
	}
	if path, err := exec.LookPath("terminal-notifier"); err == nil {
		exec.Command(path, "-title", title, "-message", message).Run()
		return
	}
	script := "display notification " + quoteAppleScript(message) +
		" with title " + quoteAppleScript(title)
	exec.Command("osascript", "-e", script).Run()
}

func quoteAppleScript(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
