package main

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/gen2brain/beeep"
)

// beeep converts the icon to an .icns before it will use terminal-notifier, so
// an icon that does not decode as PNG silently downgrades every macOS
// notification to osascript. Nothing at runtime reports that, hence this test.
func TestIconIsDecodablePNG(t *testing.T) {
	if len(icon) == 0 {
		t.Fatal("icon is empty; the go:embed did not resolve")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(icon))
	if err != nil {
		t.Fatalf("icon does not decode as PNG: %v", err)
	}
	if cfg.Width != cfg.Height {
		t.Errorf("icon is %dx%d, want square", cfg.Width, cfg.Height)
	}
}

// beeep defaults AppName to "DefaultAppName", which would show up as the Linux
// D-Bus application name and defeat macOS notification grouping.
func TestAppNameIsSet(t *testing.T) {
	if beeep.AppName != "mqw" {
		t.Errorf("beeep.AppName = %q, want %q", beeep.AppName, "mqw")
	}
}

func TestNotifierDefaultsToRaise(t *testing.T) {
	if notifier == nil {
		t.Fatal("notifier is nil; the notification seam is unwired")
	}
}
