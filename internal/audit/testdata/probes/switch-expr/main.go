package main

import "os"

func main() {
	n := 3
	switch n {
	case 3:
		os.Exit(7)
	default:
		os.Exit(1)
	}
}
