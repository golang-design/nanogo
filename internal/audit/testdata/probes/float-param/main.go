package main

import "os"

func half(x float64) float64 { return x / 2 }

func main() {
	os.Exit(int(half(14.0)))
}
