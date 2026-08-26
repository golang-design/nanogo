package main

import (
	_ "embed"
	"os"
)

//go:embed seven.txt
var seven string

func main() {
	os.Exit(len(seven))
}
