//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func findBrowser() string {
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		apps := []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
			"Chromium.app/Contents/MacOS/Chromium",
		}
		for _, a := range apps {
			for _, base := range []string{"/Applications", filepath.Join(home, "Applications")} {
				p := filepath.Join(base, a)
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
		return ""
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func hideConsole(cmd *exec.Cmd) {}

func openDefault(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

func allowForeground() {}

func bringWindowToFront() string { return "" }
