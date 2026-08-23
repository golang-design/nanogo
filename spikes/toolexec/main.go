package main

import (
	"os"
	"os/exec"
	"strings"
)

func main() {
	log, _ := os.OpenFile(os.Getenv("PT_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	log.WriteString(strings.Join(os.Args[1:], " ") + "\n")
	log.Close()
	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		os.Exit(1)
	}
}
