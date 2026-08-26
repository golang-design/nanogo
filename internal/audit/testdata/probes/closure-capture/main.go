package main

import "os"

func main() {
	n := 3
	f := func() int { return n + 4 }
	if f() == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
