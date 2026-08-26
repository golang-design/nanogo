package main

import "os"

func main() {
	n := 0
	add := func(d int) { n = n + d }
	add(3)
	add(4)
	if n == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
