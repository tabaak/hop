package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCurrentVersion(t *testing.T) {
	if got := currentVersion(); got != "v1.0.0" {
		t.Errorf("currentVersion() = %q, want %q", got, "v1.0.0")
	}

	origVer := version
	defer func() { version = origVer }()

	version = "v1.2.3"
	if got := currentVersion(); got != "v1.2.3" {
		t.Errorf("currentVersion() = %q, want %q", got, "v1.2.3")
	}

	origV := Version
	defer func() { Version = origV }()

	Version = "v2.0.0"
	if got := currentVersion(); got != "v2.0.0" {
		t.Errorf("currentVersion() = %q, want %q", got, "v2.0.0")
	}
}

func captureOutput(f func()) string {
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	w.Close()
	os.Stdout = orig

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return strings.TrimSpace(buf.String())
}

func TestRunVersion(t *testing.T) {
	out := captureOutput(func() {
		runVersion()
	})
	if out != "hopd v1.0.0 (protocol 1)" {
		t.Errorf("runVersion() = %q, want %q", out, "hopd v1.0.0 (protocol 1)")
	}
}
