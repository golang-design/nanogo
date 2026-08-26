package main

import "os"

func main() {
	m := make(map[int]int)
	m[1] = 7
	os.Exit(m[1])
}
