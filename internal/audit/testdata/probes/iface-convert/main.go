package main

import "os"

func take(v any) int {
	if v == nil {
		return 1
	}
	return 7
}

func main() {
	os.Exit(take(3))
}
