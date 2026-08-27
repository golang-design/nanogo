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

// The map group of specs/031-runtime-lowering.md, run as a program.
//
// A unit test can read the call a map row lowers to and cannot say whether the
// runtime agrees with the descriptor the call was given. Everything about a map
// is in that descriptor: the key's hash, the slot group's size, the offset and
// the stride of the key and the element inside a group, and the pointer map the
// collector reads. Every one of those is a number this compiler computed, and a
// number one off makes the runtime read a key where an element is.
//
// So the program is written to fail loudly rather than quietly. The values are
// pointers and the only reference to any of them is the map, so a group whose
// pointer map is wrong lets the collector free a value the map holds. It runs
// under GODEBUG=gccheckmark=1, which marks a second time with the world stopped
// and compares, so a pointer the map misses is a crash where the mistake is
// rather than a leak, and under clobberfree=1, so a freed object read through a
// stale pointer holds a recognisable pattern rather than its old contents.
// GOGC=1 collects as often as it can.
//
// runtime.GC is called inside the iteration on purpose. The maps.Iter this
// compiler puts in the frame holds five pointers into the table, and a slot
// described as holding none lets the collector free the table an iteration is
// walking.
//
// Every check divides by zero and the process dies, as in helloProgram: the
// exit status is the assertion.
const mapProgram = `package main

import "runtime"

type node struct {
	a, b, c, d int
	next       *node
}

var total int

//go:noinline
func churn() {
	for i := 0; i < 4096; i++ {
		_ = &node{a: i}
	}
}

// pair is the only place a value is built, and nothing but the map holds what
// it returns.
//
//go:noinline
func pair(n int) *node {
	v := &node{a: n}
	v.next = &node{a: n + 1}
	return v
}

//go:noinline
func key(i int) string {
	const names = "abcdefghijklmnop"
	return names[i : i+1]
}

func fill(m map[string]*node, n int) {
	for i := 0; i < n; i++ {
		m[key(i)] = pair(i)
	}
}

//go:noinline
func add(v *node) {
	total = total + v.next.a - v.a
}

func walk(m map[string]*node) {
	for _, v := range m {
		runtime.GC()
		add(v)
	}
}

func drop(m map[string]*node, n int) {
	for i := 0; i < n; i++ {
		delete(m, key(i))
	}
}

func main() {
	m := map[string]*node{"z": pair(100)}
	fill(m, 16)
	churn()
	runtime.GC()
	churn()

	// Every value the map holds is still the value that was put there.
	walk(m)
	if total != len(m) {
		crash()
	}
	if v, ok := m["z"]; !ok || v.next.a-v.a != 1 {
		crash()
	}

	// Half the keys leave, and what is left survives the next collection.
	drop(m, 8)
	churn()
	runtime.GC()
	if len(m) != 9 {
		crash()
	}
	total = 0
	walk(m)
	if total != 9 {
		crash()
	}
	if v := m["p"]; v.next.a-v.a != 1 {
		crash()
	}

	clear(m)
	if len(m) != 0 {
		crash()
	}
}

//go:noinline
func crash() {
	d := 0
	d = d / d
}
`

// TestToolexecKeepsAMapsValuesThroughACollection is the evidence that the slot
// group descriptor this compiler writes describes the map to the collector.
func TestToolexecKeepsAMapsValuesThroughACollection(t *testing.T) {
	h := setup(t, map[string]string{
		"go.mod":  "module nanogo.example/mapgc\n\ngo 1.27\n",
		"main.go": mapProgram,
	}, []string{"main"})

	if out, err := h.build(t, "-o", "mapgc", "."); err != nil {
		t.Fatalf("go build -toolexec=nanogo: %v\n%s", out, err)
	}
	if lines := h.decisions(t); !compiled(lines, "main") {
		t.Fatalf("nanogo delegated the main package:\n%s", strings.Join(lines, "\n"))
	}
	cmd := exec.Command(filepath.Join(h.mod, "mapgc"))
	cmd.Env = append(os.Environ(), "GODEBUG=gccheckmark=1,clobberfree=1", "GOGC=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the collector or the program rejected what the map held: %v\n%s", err, b)
	}
}
