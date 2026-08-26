package main

import "os"

func main() {
	a := 40
	b := 2
	c := a*b/2 - 33
	d := int32(c)
	e := int64(d)
	if e == 7 && a > b && a != b {
		os.Exit(7)
	}
	os.Exit(1)
}
