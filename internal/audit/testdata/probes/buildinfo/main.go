package main

import (
	"os"
	"runtime/debug"
)

func main() {
	if _, ok := debug.ReadBuildInfo(); ok {
		os.Exit(7)
	}
	os.Exit(1)
}
