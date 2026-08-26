package main

import "os"

type point struct{ x int }

func main() {
	ps := make([]point, 7)
	os.Exit(len(ps))
}
