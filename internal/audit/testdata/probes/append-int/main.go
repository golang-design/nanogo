package main

import "os"

func main() {
	xs := []int{1, 2}
	xs = append(xs, 4)
	total := 0
	for _, x := range xs {
		total = total + x
	}
	os.Exit(total)
}
