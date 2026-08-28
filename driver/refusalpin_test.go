// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusalPins are the phrases the help text uses to describe a refusal, each
// beside the probe that shows the behaviour.
//
// TestHelpStatesTheLimits pins that each phrase is present. This table pins the
// other half: that the probe beside it is still refused. A phrase describing a
// construct that now compiles is a lie, and nothing else in this repository
// catches one.
//
// That gap is not hypothetical. In one sitting, five separate claims went stale
// with every gate green: fmt.Println, a method value, defer of a method of an
// interface, a closure capturing a literal type, and every map and channel
// operation were all documented as refused after each of them started working.
// The ratchet reported each probe rising and nothing connected a rise to the
// paragraph it falsified, so each was found by hand, and fmt.Println was found
// only because a reader would have been told to restructure their program
// around a limitation that no longer exists.
var refusalPins = []struct {
	phrase string // what the help says
	probe  string // the probe in internal/audit/testdata/probes that shows it
}{
	{"print or println", "defer-builtin"},
	// A method of a generic type, and not "generic function": the help says
	// both and a generic function this package declares compiles now. That pin
	// read "generic function", which is a substring of the sentence describing
	// what works, so it went on passing while the probe beside it rose. A pin
	// has to name the phrase that describes the refusal and nothing wider.
	{"A method of a generic type", "generic-method"},
	{`imports "C"`, "cgo-import"},
	{"assembly", "asm-package"},
	{"go:embed", "embed-directive"},
}

// TestARefusalPinNamesAProbeThatIsStillRefused reads the probe ratchet and
// fails when a pinned phrase describes a construct that compiles.
//
// It reads the ratchet rather than running the corpus, because the ratchet is
// the recorded answer and this test has to be fast enough to run beside the
// other help tests. A probe missing from the ratchet is a failure too: a pin
// naming a probe that no longer exists guards nothing.
func TestARefusalPinNamesAProbeThatIsStillRefused(t *testing.T) {
	path := filepath.Join("..", "internal", "audit", "testdata", "ratchet.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the probe ratchet is what says whether a refusal is still one: %v", err)
	}
	class := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "probe" {
			class[f[2]] = f[1]
		}
	}
	if len(class) == 0 {
		t.Fatalf("%s records no probe, so this test proves nothing", path)
	}
	for _, p := range refusalPins {
		got, ok := class[p.probe]
		if !ok {
			t.Errorf("the pin %q names probes/%s and the ratchet has no such probe, so the pin guards nothing",
				p.phrase, p.probe)
			continue
		}
		if got != "refused" {
			t.Errorf("the help says %q and probes/%s is %q, so the help describes a construct that compiles: correct the text and drop the pin",
				p.phrase, p.probe, got)
		}
		if !strings.Contains(Help, p.phrase) {
			t.Errorf("the pin %q is not in the help text", p.phrase)
		}
	}
}
