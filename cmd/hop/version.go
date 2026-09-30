package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"

	"hop.vokh.dev/internal/client"
	"hop.vokh.dev/internal/proto"
)

// Version information.
// Default is "v1.0.0". Can be overridden at build time via:
//
//	go build -ldflags "-X main.version=..."
//
// Both `version` and `Version` are supported for linker overrides.
var (
	version = "v1.0.0"
	Version = ""
)

func runVersion(args []string) {
	fs := flag.NewFlagSet("hop version", flag.ExitOnError)
	short := fs.Bool("short", false, "print only the version number")
	fs.BoolVar(short, "s", false, "print only the version number")
	var dummy bool
	fs.BoolVar(&dummy, "v", false, "")
	fs.BoolVar(&dummy, "version", false, "")
	serverAddr := fs.String("server", envOr("HOP_SERVER", "hop.vokh.dev:7443"), "hop server control address")
	token := fs.String("token", os.Getenv("HOP_TOKEN"), "agent token (or set HOP_TOKEN)")
	noTLS := fs.Bool("no-tls", false, "connect without TLS (local development only)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hop version [-s|--short] [--server addr] [--token token] [--no-tls]\n")
	}
	fs.Parse(args)

	v := currentVersion()
	if *short {
		fmt.Println(v)
		return
	}
	fmt.Printf("hop %s (protocol %s)\n", v, proto.Version)

	// The server's half needs a token, since it is only told to someone who
	// may use it. Without one this stays the offline command it always was.
	if *token == "" {
		return
	}
	_, info, err := client.List(client.Config{
		Server:  *serverAddr,
		Token:   *token,
		TLS:     !*noTLS,
		Release: v,
	})
	if err != nil {
		// The client's own version is already out, which is what was asked
		// for; not reaching the server doesn't make that a failure.
		fmt.Fprintf(os.Stderr, "hop: could not ask %s for its version: %v\n", *serverAddr, err)
		return
	}
	release := info.Release
	if release == "" {
		release = "older than v1.1.0"
	}
	fmt.Printf("hopd %s (protocol %s) at %s\n", release, proto.ServerProtocol(info.Protocol), *serverAddr)
	if note := serverBehind(info, v); note != "" {
		fmt.Fprintf(os.Stderr, "hop: note: %s\n", note)
	}
}

func currentVersion() string {
	if Version != "" {
		return Version
	}
	if version != "" && version != "(devel)" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return "v1.0.0"
}
