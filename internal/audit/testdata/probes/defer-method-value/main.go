package main

import "os"

type ender struct{ code int }

func (e ender) end() { os.Exit(e.code) }

func main() {
	e := ender{code: 7}
	defer e.end()
}
