package main

import "os"

var caught = 0

func guard() {
	if recover() != nil {
		caught = 7
	}
}

func boom() {
	defer guard()
	var xs []int
	_ = xs[3]
}

func main() {
	boom()
	os.Exit(caught)
}
