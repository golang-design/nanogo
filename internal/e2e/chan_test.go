// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The channel group of specs/031-runtime-lowering.md, run as a program.
//
// A unit test can read the call a send lowers to and cannot say whether the
// two goroutines meet. This program makes the claim that needs a scheduler:
// the receive runs before the value exists, so it blocks, and the send in the
// other goroutine is what wakes it. A compiler that emitted the calls with the
// operands the wrong way round would still pass every tree assertion and hang
// here.
//
// The buffered half needs no second goroutine and is the other shape: the send
// completes into the buffer and the receive takes it back out.
//
// The exit status is the assertion, as in helloProgram. A wrong answer divides
// by zero and the process dies.
const chanProgram = `package main

func handoff() int {
	c := make(chan int)
	go func() { c <- 7 }()
	return <-c
}

func buffered() int {
	c := make(chan int, 2)
	c <- 3
	c <- 4
	return <-c + <-c
}

func commaOK() int {
	c := make(chan int, 1)
	c <- 5
	v, ok := <-c
	if !ok {
		return 0
	}
	close(c)
	_, open := <-c
	if open {
		return 0
	}
	return v
}

// lenCap is the row a load would get wrong. A nil channel has length and
// capacity zero and no hchan to read them from, so the compiler that read the
// words itself would fault on the first line.
func lenCap() int {
	var nilc chan int
	if len(nilc) != 0 || cap(nilc) != 0 {
		return 0
	}
	c := make(chan int, 3)
	c <- 1
	c <- 2
	return len(c)*10 + cap(c)
}

func main() {
	d := handoff() - 7
	if d != 0 {
		d = d / (d - d)
	}
	d = buffered() - 7
	if d != 0 {
		d = d / (d - d)
	}
	d = commaOK() - 5
	if d != 0 {
		d = d / (d - d)
	}
	d = lenCap() - 23
	if d != 0 {
		d = d / (d - d)
	}
}
`

// The program that sends pointers through a channel across a collection.
//
// The element of a send and of a receive crosses the call as the address of a
// frame slot, and the collector reads that slot through the locals bitmap
// while the call is blocked inside the runtime. A slot described as holding no
// pointer would have the value freed while the channel still held it, and the
// channel's own word is the same question one level up: an hchan described as
// a number is an hchan the collector may free while a goroutine is parked in
// it.
//
// churn gives the collector something to sweep, and every value that survives
// survives because something the compiler described kept it.
const chanGCProgram = `package main

import "runtime"

type node struct {
	a, b, c, d int
	next       *node
}

//go:noinline
func churn() {
	for i := 0; i < 4096; i++ {
		_ = &node{a: i}
	}
}

func produce(c chan *node, n int) {
	v := &node{a: n}
	v.next = &node{a: n + 1}
	churn()
	c <- v
}

func main() {
	c := make(chan *node, 1)
	total := 0
	for i := 0; i < 64; i++ {
		go produce(c, i)
		churn()
		runtime.GC()
		v := <-c
		total = total + v.next.a - v.a
	}
	d := total - 64
	if d != 0 {
		d = d / (d - d)
	}
}
`

// TestToolexecHandsAValueBetweenGoroutines runs the channel rows.
func TestToolexecHandsAValueBetweenGoroutines(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/chans\n\ngo 1.27\n",
		"main.go": chanProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "chans", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	if b, err := exec.Command(filepath.Join(h.mod, "chans")).CombinedOutput(); err != nil {
		t.Fatalf("the program nanogo compiled did not run: %v\n%s", err, b)
	}
}

// TestToolexecKeepsAChannelsElementsThroughACollection is the evidence that
// the frame slot a channel operation names is described to the collector.
//
// It runs under GODEBUG=gccheckmark=1, which marks a second time with the
// world stopped and compares, so a pointer the map misses is a crash where the
// mistake is rather than a leak, and under clobberfree=1, so a freed object
// read through a stale pointer holds a recognisable pattern rather than its
// old contents. GOGC=1 collects as often as it can.
func TestToolexecKeepsAChannelsElementsThroughACollection(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/changc\n\ngo 1.27\n",
		"main.go": chanGCProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "changc", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	cmd := exec.Command(filepath.Join(h.mod, "changc"))
	cmd.Env = append(os.Environ(), "GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the collector or the program rejected what the channel held: %v\n%s", err, b)
	}
}
