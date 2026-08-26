package main

import "os"

func main() {
	n := 0
loop:
	for i := 0; i < 10; i++ {
		switch {
		case i == 3:
			n = n + 1
			fallthrough
		case i == 4:
			n = n + 1
			continue
		case i == 8:
			break loop
		}
		n = n + 1
	}
	if n < 0 {
		goto out
	}
	if n == 9 {
		os.Exit(7)
	}
out:
	os.Exit(1)
}
