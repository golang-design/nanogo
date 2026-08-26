package main

import "os"

func main() {
	var v any = 7
	switch n := v.(type) {
	case int:
		os.Exit(n)
	default:
		os.Exit(1)
	}
}
