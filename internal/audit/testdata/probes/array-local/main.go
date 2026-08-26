package main

import "os"

func main() {
	var a [4]int
	a[2] = 7
	os.Exit(a[2])
}
