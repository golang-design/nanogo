package main

import "os"

func main() {
	var v any = 7
	if n, ok := v.(int); ok {
		os.Exit(n)
	}
	os.Exit(1)
}
