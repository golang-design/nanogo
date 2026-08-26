package main

import "os"

func end(code int) { os.Exit(code) }

func main() {
	n := 7
	defer end(n)
	n = 1
}
