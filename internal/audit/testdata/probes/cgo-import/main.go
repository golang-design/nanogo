package main

/*
int seven(void) { return 7; }
*/
import "C"

import "os"

func main() {
	os.Exit(int(C.seven()))
}
