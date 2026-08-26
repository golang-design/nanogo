package main

import "os"

func main() {
	xs := []int{1, 2, 3, 4}
	ys := make([]int, 4)
	for i := range xs {
		ys[i] = xs[i]
	}
	zs := ys[1:3]
	if len(zs) == 2 && zs[0] == 2 && len(ys) == 4 {
		os.Exit(7)
	}
	os.Exit(1)
}
