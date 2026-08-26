package main

import "os"

func boom(e error) {
	if e != nil {
		panic(e)
	}
}

func main() {
	boom(nil)
	os.Exit(7)
}
