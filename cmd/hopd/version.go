package main

import (
	"fmt"
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

func runVersion() {
	fmt.Printf("hopd %s\n", currentVersion())
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
