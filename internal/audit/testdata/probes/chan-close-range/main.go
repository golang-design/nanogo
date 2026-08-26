package main

import "os"

func main() {
	c := make(chan int, 2)
	c <- 3
	c <- 4
	close(c)
	total := 0
	for v := range c {
		total = total + v
	}
	os.Exit(total)
}
