package main

import (
	"fmt"
	"os"

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
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hopd mint <label>\n\nExample: hopd mint laptop")
		os.Exit(2)
	}
	label := args[0]
	if !tokens.LabelOK(label) {
		fmt.Fprintf(os.Stderr, "hopd: %q is not a valid label (1-32 chars of a-z, 0-9, '-')\n", label)
		os.Exit(2)
	}

	secret, err := tokens.Mint()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hopd: generating a token: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf(`Token for %q. Copy it now — it is not stored anywhere and cannot be shown again:

  %s

Add this line to the tokens file; hopd picks it up within seconds, no restart:

  %s  sha256:%s

On the device, put the token in ~/.zshrc or the equivalent:

  export HOP_TOKEN="<the token above>"
`, label, secret, label, tokens.Hash(secret))
}
