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
	phrase: "loadbearing",
	why: "a metaphor that names no part of the thing it describes. " +
		"Say what actually depends on what: \"the linker collects symbols by this prefix\" " +
		"rather than \"this prefix is significant\".",
}}

// normalise reduces text to the form a phrase is matched in.
//
// It drops case, whitespace, hyphens and comment markers, so that one entry in
// bannedPhrases catches every spelling of it. The phrase this gate was written
// for was already in the repository three ways: hyphenated, spaced, and
// hyphenated across a line break in a comment, where the continuation carries
// a "//" of its own. A gate that matched one of those passed on the other two,
// and the spaced form is the one that appears mid-sentence, where the hyphen
// reads wrong. It was found by a reader of the first version rather than by
// the gate itself, which is the argument for normalising rather than for
// adding a second entry per spelling.
//
// The cost is a false positive when two unrelated words abut, and the message
// prints the line so that a reader sees which it is.
func normalise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case '-', '/', '*', '_', ' ', '\t', '\n', '\r':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

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
		flat := normalise(string(b))
		for _, p := range bannedPhrases {
			if !strings.Contains(flat, p.phrase) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			// Report the line. A phrase broken across a line break matches
			// only when the two lines are joined, so each line is tried with
			// its successor and the first of the pair is named.
			lines := strings.Split(string(b), "\n")
			hit := false
			for i := range lines {
				window := lines[i]
				if i+1 < len(lines) {
					window += "\n" + lines[i+1]
				}
				if !strings.Contains(normalise(window), p.phrase) {
					continue
				}
				found = append(found, rel+":"+itoa(i+1)+": "+strings.TrimSpace(lines[i])+
					"\n\t\t"+p.why)
				hit = true
			}
			if !hit {
				// The file matches and no pair of lines does, so the phrase is
				// spread wider than a wrap. Name the file rather than nothing.
				found = append(found, rel+": "+p.phrase+" spans more than two lines\n\t\t"+p.why)
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
