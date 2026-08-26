package main

import "os"

func main() {
	n, err := os.Stdout.Write([]byte("seven\n"))
	if err != nil || n != 6 {
		os.Exit(1)
	}
	os.Exit(7)
}
