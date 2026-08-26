package main

import "os"

func pick[T any](a, b T, first bool) T {
	if first {
		return a
	}
	return b
}

func main() {
	os.Exit(pick(7, 1, true))
}
