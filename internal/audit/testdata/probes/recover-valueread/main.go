package main

import "os"

var caught = 0

func guard() {
	if r := recover(); r != nil {
		caught = 7
	}
}

func run() {
	defer guard()
	panic("boom")
}

func main() {
	run()
	os.Exit(caught)
}
