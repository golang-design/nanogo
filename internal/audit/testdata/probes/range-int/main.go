package main

import "os"

func main() {
	n := 0
	for i := range 5 {
		n = n + i
	}
	if n == 10 {
		os.Exit(7)
	}
	os.Exit(1)
}
