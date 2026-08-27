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

// closeRange drains a closed channel. The loop has no bound: it leaves when
// the receive says the channel is closed and empty, and a compiler that left
// it any other way either hangs or reads one value past the end.
func closeRange() int {
	c := make(chan int, 3)
	c <- 3
	c <- 4
	c <- 5
	close(c)
	total := 0
	for v := range c {
		total = total + v
	}
	// A break the program writes binds to the same loop the receive does.
	d := make(chan int, 2)
	d <- 1
	d <- 2
	close(d)
	for v := range d {
		total = total + v
		break
	}
	return total
}

// selectOne is a select with one ready case and nothing else. It has to pick
// that case, and the value has to arrive in the clause's own variable.
func selectOne() int {
	c := make(chan int, 1)
	c <- 7
	select {
	case v := <-c:
		return v
	}
}

// selectDefault is the non-blocking shape. Nothing is ready, so selectgo
// reports the default by returning an index below zero, and the switch's
// default arm is what that reaches.
//
// Each arm returns rather than assigning one variable, and that is a limit of
// the register allocator and not of this row: a three-arm select whose arms
// merge into one variable makes a phi with three spilled operands, and
// ssa/regalloc.go reserves two scratch registers.
func selectDefault() int {
	c := make(chan int)
	select {
	case <-c:
		return 1
	case c <- 1:
		return 2
	default:
		return 3
	}
}

// selectSendAndReceive puts a send and a receive in one select, which is the
// shape that needs the two groups of the case array in the right order: the
// sends first and the receives after, with a count of each.
func selectSendAndReceive() int {
	full := make(chan int, 1)
	full <- 1
	empty := make(chan int, 1)
	total := 0
	for i := 0; i < 2; i++ {
		select {
		case v := <-full:
			total = total + v*10
		case empty <- 5:
			total = total + 100
		}
	}
	return total + <-empty
}

// selectBlank is the shape the clause list takes apart wrongly if the
// communication is looked for from the front. Both destinations are blank, so
// ir.Build puts the receive into temporaries and copies out of them, and the
// last statement of the clause is a copy rather than the receive.
func selectBlank() int {
	c := make(chan int, 1)
	d := make(chan int, 1)
	c <- 1
	select {
	case _, _ = <-c:
		// The operand of a send is evaluated on entry, so the receive from d
		// below runs whether or not this clause is the one chosen.
	}
	d <- 2
	e := make(chan int, 1)
	select {
	case e <- <-d:
	}
	return <-e
}

// selectClosed reads the second result. A closed channel is always ready, and
// the receive reports that no value arrived.
func selectClosed() int {
	c := make(chan int)
	close(c)
	select {
	case v, ok := <-c:
		if ok || v != 0 {
			return 0
		}
		return 9
	}
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
	d = closeRange() - 13
	if d != 0 {
		d = d / (d - d)
	}
	d = selectOne() - 7
	if d != 0 {
		d = d / (d - d)
	}
	d = selectDefault() - 3
	if d != 0 {
		d = d / (d - d)
	}
	d = selectSendAndReceive() - 115
	if d != 0 {
		d = d / (d - d)
	}
	d = selectClosed() - 9
	if d != 0 {
		d = d / (d - d)
	}
	d = selectBlank() - 2
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

// pick is the select half of the same claim. The case array holds the channel
// and the address of the element, and both words are pointers: an array
// described as holding none lets the collector free the channel a goroutine is
// parked in, or free what the element points at.
//
// The receive lands in a frame slot of the row's own and the slot is copied
// into v in the chosen arm, so the slot is live across the call and holds a
// pointer the collector must follow.
func pick(c chan *node, spare chan *node) int {
	churn()
	select {
	case v := <-c:
		return v.next.a - v.a
	case spare <- &node{a: 1}:
		<-spare
		return 1
	}
}

func straight() int {
	c := make(chan *node, 1)
	total := 0
	for i := 0; i < 64; i++ {
		go produce(c, i)
		churn()
		runtime.GC()
		v := <-c
		total = total + v.next.a - v.a
	}
	return total
}

func chosen() int {
	c := make(chan *node, 1)
	spare := make(chan *node, 1)
	total := 0
	for i := 0; i < 64; i++ {
		go produce(c, i)
		runtime.GC()
		total = total + pick(c, spare)
	}
	return total
}

func main() {
	d := straight() + chosen() - 128
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
