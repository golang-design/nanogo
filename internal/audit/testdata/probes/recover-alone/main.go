package main

import "os"

func guard() { recover() }

func main() {
	defer guard()
	os.Exit(7)
}
