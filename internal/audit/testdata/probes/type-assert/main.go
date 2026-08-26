package main

import "os"

func main() {
	var v any = 7
	n := v.(int)
	os.Exit(n)
}
