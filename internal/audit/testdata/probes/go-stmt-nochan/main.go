package main

import (
	"os"
	"time"
)

var n = 0

func bump() { n = 7 }

func main() {
	go bump()
	time.Sleep(100 * time.Millisecond)
	os.Exit(n)
}
