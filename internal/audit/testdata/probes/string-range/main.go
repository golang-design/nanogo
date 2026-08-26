package main

import "os"

func main() {
	n := 0
	for range "abcdefg" {
		n = n + 1
	}
	if n == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
