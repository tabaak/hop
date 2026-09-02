package main

import (
	"os"
	"os/exec"
	"runtime"
)

// openBrowser asks the desktop to show url, however this platform does that,
// and returns without waiting for the browser to exit.
//
// The page's address is printed in the banner either way, so a failure to
// launch — a headless machine, a bare SSH session, no desktop at all — costs
// the user nothing and is not worth a warning of its own: the URL on screen
// already says what to do by hand.
//
// $BROWSER overrides the platform default when set, which is the convention
// browsers and tools already agree on. It is taken as a command name run with
// the URL as its argument; anything fancier (fallback lists, %s placeholders)
// is more ceremony than a debugging page needs.
func openBrowser(url string) {
	spawnBrowser(browserCommand(url))
}

// browserCommand builds the command that shows url in a browser, or nil when
// this platform has no answer.
func browserCommand(url string) *exec.Cmd {
	if b := os.Getenv("BROWSER"); b != "" {
		return exec.Command(b, url)
	}
	name, args := pickOpener(runtime.GOOS)
	if name == "" {
		return nil
	}
	return exec.Command(name, append(args, url)...)
}

// pickOpener maps a platform to whatever opens a URL in its browser. An empty
// name means there is no known answer there.
func pickOpener(goos string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler"}
	case "linux":
		return "xdg-open", nil
	default:
		return "", nil
	}
}

// spawnBrowser starts cmd without waiting for it: a browser outlives the
// command that asked for it. It does reap whenever it exits, so it doesn't sit
// as a zombie for as long as the inspector keeps running.
func spawnBrowser(cmd *exec.Cmd) {
	if err := cmd.Start(); err != nil {
		return // see openBrowser: the banner is the fallback
	}
	go func() { _ = cmd.Wait() }()
}
