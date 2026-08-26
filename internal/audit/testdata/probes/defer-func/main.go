package main

import "os"

func bye() { os.Exit(7) }

func main() {
	defer bye()
}
