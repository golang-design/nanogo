package main

import "os"

type coder interface{ code() int }

type seven struct{}

func (seven) code() int { return 7 }

func main() {
	var c coder = seven{}
	os.Exit(c.code())
}
