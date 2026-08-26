package main

import "os"

var xs = []int{1, 2, 4}

func main() {
	total := 0
	for _, x := range xs {
		total = total + x
	}
	os.Exit(total)
}
