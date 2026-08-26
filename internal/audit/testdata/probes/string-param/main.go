package main

import "os"

func size(s string) int { return len(s) }

func label() string { return "seven!!" }

func main() {
	os.Exit(size(label()))
}
