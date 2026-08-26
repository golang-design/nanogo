package main

import "os"

func main() {
	if os.Stdout == nil {
		os.Exit(1)
	}
	os.Exit(7)
}
