package main

import "os"

func main() {
	p := new(int)
	*p = 7
	if *p == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
