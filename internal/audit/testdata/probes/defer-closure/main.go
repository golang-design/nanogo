package main

import "os"

func main() {
	n := 3
	defer func() { os.Exit(n + 4) }()
	n = 3
}
