package main

import "os"

func main() {
	a, b := 3, 7
	a, b = b, a
	if a == 7 && b == 3 {
		os.Exit(7)
	}
	os.Exit(1)
}
