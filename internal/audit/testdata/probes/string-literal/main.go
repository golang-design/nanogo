package main

import "os"

func main() {
	s := "hello"
	if len(s) == 5 {
		os.Exit(7)
	}
	os.Exit(1)
}
