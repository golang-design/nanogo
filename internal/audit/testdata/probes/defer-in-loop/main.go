package main

import "os"

var n = 0

func bump() { n = n + 1 }

func run() {
	for i := 0; i < 7; i++ {
		defer bump()
	}
}

func main() {
	run()
	os.Exit(n)
}
