package main

import "os"

func main() {
	m := map[int]int{1: 3, 2: 4}
	total := 0
	for _, v := range m {
		total = total + v
	}
	os.Exit(total)
}
