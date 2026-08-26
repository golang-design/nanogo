package main

import "os"

func adder(base int) func(int) int {
	return func(d int) int { return base + d }
}

func main() {
	if adder(3)(4) == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
