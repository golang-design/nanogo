package main

import "os"

type coder interface{ code() int }

func main() {
	cs := make([]coder, 7)
	os.Exit(len(cs))
}
