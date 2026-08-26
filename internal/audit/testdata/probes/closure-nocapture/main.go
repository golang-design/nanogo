package main

import "os"

func main() {
	f := func(a int) int { return a + 3 }
	if f(4) == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
