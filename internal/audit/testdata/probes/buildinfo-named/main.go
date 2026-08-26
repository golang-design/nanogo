package main

import (
	"os"
	"runtime/debug"
)

func main() {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		os.Exit(1)
	}
	os.Exit(7)
}
