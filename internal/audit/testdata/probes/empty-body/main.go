package main

import "os"

func nothing() {}

func main() {
	nothing()
	os.Exit(7)
}
