package main

import "os"

var log = 0

func first()  { log = log*10 + 1 }
func second() { log = log*10 + 2 }
func report() { os.Exit(log) }

func main() {
	defer report()
	defer second()
	defer first()
}
