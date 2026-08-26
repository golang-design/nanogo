package main

import "os"

func main() {
	bs := []byte("hi")
	s := string(bs)
	if len(s) == 2 {
		os.Exit(7)
	}
	os.Exit(1)
}
