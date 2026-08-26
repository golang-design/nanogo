package main

import "os"

func main() {
	c := make(chan int, 1)
	c <- 7
	select {
	case v := <-c:
		os.Exit(v)
	}
}
