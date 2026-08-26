package main

import "os"

func main() {
	xs := []int{1, 2}
	ys := []int{4}
	xs = append(xs, ys...)
	os.Exit(len(xs) + 4)
}
