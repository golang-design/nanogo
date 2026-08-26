package main

import "os"

var total = 0

func init() { total = 7 }

func main() {
	os.Exit(total)
}
