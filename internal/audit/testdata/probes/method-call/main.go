package main

import "os"

type counter struct{ n int }

func (c *counter) add(d int) { c.n = c.n + d }
func (c counter) get() int   { return c.n }

func main() {
	var c counter
	c.add(7)
	if c.get() == 7 {
		os.Exit(7)
	}
	os.Exit(1)
}
