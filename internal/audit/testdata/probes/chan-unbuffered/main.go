package main

import "os"

func main() {
	c := make(chan int)
	go func() { c <- 7 }()
	os.Exit(<-c)
}
