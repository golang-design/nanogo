package main

import "os"

func guard() {
	recover()
}

func run() {
	defer guard()
	panic("boom")
}

func main() {
	run()
	os.Exit(7)
}
