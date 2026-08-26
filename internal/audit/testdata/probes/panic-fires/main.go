package main

import "errors"

func boom(e error) {
	if e != nil {
		panic(e)
	}
}

func main() {
	boom(errors.New("boom"))
}
