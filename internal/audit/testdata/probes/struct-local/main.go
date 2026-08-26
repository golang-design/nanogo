package main

import "os"

type point struct {
	x int
	y int
}

func main() {
	p := point{x: 3, y: 4}
	p.y = p.y + 3
	if p.x+p.y == 10 {
		os.Exit(7)
	}
	os.Exit(1)
}
