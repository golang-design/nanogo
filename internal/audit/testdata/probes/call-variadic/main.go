package main

import "os"

func sum(xs ...int) int {
	total := 0
	for _, x := range xs {
		total = total + x
	}
	return total
}

func main() {
	if sum(1, 2, 4) == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
