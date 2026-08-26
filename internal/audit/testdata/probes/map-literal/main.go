package main

import "os"

func main() {
	m := map[string]int{"a": 7}
	os.Exit(m["a"])
}
