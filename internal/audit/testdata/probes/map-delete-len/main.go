package main

import "os"

func main() {
	m := make(map[int]int)
	m[1] = 1
	m[2] = 2
	delete(m, 1)
	os.Exit(len(m) + 6)
}
