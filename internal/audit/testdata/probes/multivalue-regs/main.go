package main

import "os"

func two() (int, int) { return 3, 4 }

func main() {
	a, b := two()
	os.Exit(a + b)
}
