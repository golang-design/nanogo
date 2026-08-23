package main

import "fmt"

func target() int
func ptrdata() uintptr

func main() { fmt.Printf("target=%d ptrdata=%#x\n", target(), ptrdata()) }
