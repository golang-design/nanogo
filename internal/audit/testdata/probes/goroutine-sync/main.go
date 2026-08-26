package main

import (
	"os"
	"sync"
)

func main() {
	var wg sync.WaitGroup
	n := 0
	wg.Add(1)
	go func() {
		n = 7
		wg.Done()
	}()
	wg.Wait()
	os.Exit(n)
}
