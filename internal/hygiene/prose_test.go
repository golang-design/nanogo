// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package hygiene

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// banned is a phrase this repository has decided not to use, with the reason.
//
// The reason is part of the entry rather than part of a commit message,
// because the person who trips the gate is the person who needs it.
type banned struct {
	phrase string
	why    string
}

// bannedPhrases are the phrases removed from this repository once already.
//
// Each was swept out by hand, and each can come back the same way it arrived:
// in a paragraph someone rewrites, in a comment someone copies from another
// file, in a spec a tool regenerates. A sweep is a one-off and a gate is not,
// so the sweep is recorded here as a rule instead of as a memory.
//
// Add a phrase only when it has actually been removed. A gate that fails on
// the day it is written is a gate people learn to skip.
var bannedPhrases = []banned{{
	phrase: "load-bearing",
	why: "a metaphor that names no part of the thing it describes. " +
		"Say what actually depends on what: \"the linker collects symbols by this prefix\" " +
		"rather than \"this prefix is load-bearing\".",
}}

// TestTheBannedPhrasesStayGone reads every file this repository owns and fails
// on a phrase that was removed from it.
//
// The forked type checker is excluded. types2 is upstream source that nanogo
// maintains as a fork, and its prose must stay comparable with upstream's, so
// an editorial rule of this repository does not reach it. The spikes are
// excluded because they are separate modules kept as evidence of a decision,
// and evidence is not edited after the fact.
func TestTheBannedPhrasesStayGone(t *testing.T) {
	root := repoRoot(t)
	self := filepath.Join(root, "internal", "hygiene", "prose_test.go")

	skipDir := map[string]bool{
		"types2": true, "spikes": true, ".git": true, "testdata": true,
	}

	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md":
		default:
			return nil
		}
		// This file names every phrase it bans, so it cannot check itself.
		if path == self {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(b))
		for _, p := range bannedPhrases {
			if !strings.Contains(lower, p.phrase) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			for i, line := range strings.Split(string(b), "\n") {
				if strings.Contains(strings.ToLower(line), p.phrase) {
					found = append(found, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line)+
						"\n\t\t"+p.why)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(found) > 0 {
		t.Errorf("a phrase this repository removed has come back:\n\t%s",
			strings.Join(found, "\n\t"))
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
