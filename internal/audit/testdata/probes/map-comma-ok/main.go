package main

import "os"

func main() {
	m := make(map[int]int)
	m[1] = 7
	if v, ok := m[1]; ok {
		os.Exit(v)
	}
	os.Exit(1)
}
