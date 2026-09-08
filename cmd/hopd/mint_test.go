package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMint(t *testing.T) {
	out := captureOutput(func() {
		mint([]string{"laptop"})
	})
	if !strings.Contains(out, `Token for "laptop"`) {
		t.Fatalf("expected token for laptop, got: %s", out)
	}
	if !strings.Contains(out, "laptop  sha256:") {
		t.Fatalf("expected sha256 line, got: %s", out)
	}
}

func TestMintAppend(t *testing.T) {
	tmpDir := t.TempDir()
	tokensPath := filepath.Join(tmpDir, "tokens")

	out := captureOutput(func() {
		mint([]string{"phone", "-a", tokensPath})
	})
	if !strings.Contains(out, `Appended to `+tokensPath) {
		t.Fatalf("expected appended notice, got: %s", out)
	}

	data, err := os.ReadFile(tokensPath)
	if err != nil {
		t.Fatalf("failed to read tokens file: %v", err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "phone  sha256:") {
		t.Fatalf("tokens file does not start with phone label: %s", content)
	}
}
