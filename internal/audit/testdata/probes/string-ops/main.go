package main

import "os"

func main() {
	a := "he"
	b := "llo"
	c := a + b
	if len(c) == 5 && c[0] == 'h' && c > "abc" {
		os.Exit(7)
	}
	os.Exit(1)
}
