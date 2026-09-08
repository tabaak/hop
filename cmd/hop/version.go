package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
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
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: hop version [-s|--short]\n")
	}
	fs.Parse(args)

	v := currentVersion()
	if *short {
		fmt.Println(v)
		return
	}
	fmt.Printf("hop %s\n", v)
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
