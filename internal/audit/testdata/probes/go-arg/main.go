package main

import (
	"os"
	"time"
)

var n = 0

func set(v int) { n = v }

func main() {
	go set(7)
	time.Sleep(100 * time.Millisecond)
	os.Exit(n)
}
