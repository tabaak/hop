package main

import (
	"reflect"
	"testing"
)

func TestPickOpener(t *testing.T) {
	cases := []struct {
		goos string
		name string
		args []string
	}{
		{"darwin", "open", nil},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler"}},
		{"linux", "xdg-open", nil},
		{"plan9", "", nil}, // no known answer; the banner is the fallback
	}
	for _, c := range cases {
		name, args := pickOpener(c.goos)
		if name != c.name || !reflect.DeepEqual(args, c.args) {
			t.Errorf("%s opener = (%q, %q), want (%q, %q)", c.goos, name, args, c.name, c.args)
		}
	}
}

// $BROWSER wins over the platform default and receives the URL as its one
// argument.
func TestOpenBrowserHonoursBrowserEnv(t *testing.T) {
	t.Setenv("BROWSER", "/bin/echo")
	cmd := browserCommand("http://127.0.0.1:4040")
	if cmd == nil || cmd.Path != "/bin/echo" || !reflect.DeepEqual(cmd.Args, []string{"/bin/echo", "http://127.0.0.1:4040"}) {
		t.Fatalf("browserCommand with BROWSER=/bin/echo = %v", cmd)
	}
}
