package main

import "os"

type counter struct{ n int }

func (c counter) get() int { return c.n }

func main() {
	cs := make([]counter, 7)
	os.Exit(len(cs) + cs[0].get())
}
