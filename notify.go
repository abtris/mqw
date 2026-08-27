package main

import (
	_ "embed"
	"os"

	"github.com/gen2brain/beeep"
)

// icon is embedded rather than read from disk because mqw ships as a bare
// binary. beeep only reaches terminal-notifier when it can produce an .icns
// from a real PNG; handed an empty icon it always falls back to osascript.
//
//go:embed icon.png
var icon []byte

// notifier raises a user-visible notification. Swapped out in tests.
var notifier = raise

func init() {
	// beeep uses AppName as the D-Bus application name on Linux and as the
	// terminal-notifier -group on macOS, which makes a new notification replace
	// the previous one instead of stacking.
	beeep.AppName = "mqw"
}

// raise rings the terminal bell and raises a desktop notification. macOS uses
// terminal-notifier when it is installed and falls back to osascript; Linux
// goes over D-Bus and falls back to notify-send. The error is deliberately
// ignored: a missing notifier must not take down the watcher.
//
// The desktop call runs in a goroutine because notifyChanges runs inside
// Update, on the bubbletea loop. The first terminal-notifier invocation after
// install blocks while macOS asks for notification permission — measured at 22s
// — which would freeze the whole dashboard.
func raise(title, message string) {
	os.Stderr.WriteString("\a")
	go beeep.Notify(title, message, icon)
}
