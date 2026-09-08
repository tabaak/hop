package main

import (
	"fmt"
	"os"
	"strings"

	"hop.vokh.dev/internal/tokens"
)

// mint generates one credential and prints both halves: the secret, which goes
// to the device and is never stored here, and the file line, which goes to the
// tokens file and cannot be turned back into the secret.
//
// This exists because the tokens file is otherwise impractical to write by
// hand — the alternative is piping openssl into sha256sum and hoping no
// trailing newline crept in, which is the kind of step that ends with someone
// pasting the raw token into the file instead.
func mint(args []string) {
	var appendPath string
	var label string

	// Look for -a or --append flag
	var remaining []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-a" || arg == "--append" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				appendPath = args[i+1]
				i++
			} else {
				appendPath = envOr("HOP_TOKENS_FILE", "/etc/hop/tokens")
			}
		} else if strings.HasPrefix(arg, "--append=") {
			appendPath = strings.TrimPrefix(arg, "--append=")
		} else if strings.HasPrefix(arg, "-a=") {
			appendPath = strings.TrimPrefix(arg, "-a=")
		} else {
			remaining = append(remaining, arg)
		}
	}

	if len(remaining) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hopd mint <label> [-a [tokens-file]]\n\nExample:\n  hopd mint laptop\n  hopd mint laptop -a /etc/hop/tokens")
		os.Exit(2)
	}
	label = remaining[0]
	if !tokens.LabelOK(label) {
		fmt.Fprintf(os.Stderr, "hopd: %q is not a valid label (1-32 chars of a-z, 0-9, '-')\n", label)
		os.Exit(2)
	}

	secret, err := tokens.Mint()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hopd: generating a token: %v\n", err)
		os.Exit(1)
	}

	hash := tokens.Hash(secret)
	line := fmt.Sprintf("%s  sha256:%s\n", label, hash)

	if appendPath != "" {
		f, err := os.OpenFile(appendPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hopd: could not open %s: %v\n", appendPath, err)
			os.Exit(1)
		}
		if _, err := f.WriteString(line); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "hopd: could not write to %s: %v\n", appendPath, err)
			os.Exit(1)
		}
		f.Close()

		fmt.Printf(`Token for %q. Copy it now — it is not stored anywhere and cannot be shown again:

  %s

Appended to %s. hopd picks it up within seconds, no restart!

On the device:
  export HOP_TOKEN="%s"
`, label, secret, appendPath, secret)
		return
	}

	fmt.Printf(`Token for %q. Copy it now — it is not stored anywhere and cannot be shown again:

  %s

Add this line to the tokens file; hopd picks it up within seconds, no restart:

  %s  sha256:%s

On the device, put the token in ~/.zshrc or the equivalent:

  export HOP_TOKEN="<the token above>"
`, label, secret, label, hash)
}
