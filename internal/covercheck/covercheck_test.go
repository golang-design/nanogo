// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts content in a fresh file under the test's own directory and
// returns its path. Every case builds its own profile, so no case can be made
// to pass by another case's fixture.
func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const mod = "golang.design/x/nanogo"

// TestGate is the whole command over synthetic profiles. The cases are the
// outcomes CI depends on: a pass, a failure that names the package, an
// exclusion, a package with nothing in it, and every shape of bad input.
func TestGate(t *testing.T) {
	cases := []struct {
		name       string
		profile    string
		exclusions string // "" writes no file at all
		args       []string
		want       int
		wantStdout []string
		wantStderr []string
	}{{
		name: "above the gate",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 9 1\n" +
			mod + "/a/f.go:6.2,6.11 1 0\n",
		want:       0,
		wantStdout: []string{mod + "/a", "90.0%", "9/10", "at or above 90%"},
	}, {
		// Exactly the threshold passes. The doc comment says so, and a gate
		// that rejects its advertised number has an undocumented threshold.
		name: "below the gate names the package and the count",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 8 1\n" +
			mod + "/a/f.go:6.2,6.11 2 0\n",
		want:       1,
		wantStderr: []string{"::error::", mod + "/a", "80.0%", "8/10", "below the 90% gate"},
	}, {
		// One package must not be able to carry another. This is the reason
		// the gate is per package and not an average: the pair averages 90%.
		name: "a good package does not carry a bad one",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 10 1\n" +
			mod + "/b/f.go:3.10,5.4 8 1\n" +
			mod + "/b/f.go:6.2,6.11 2 0\n",
		want:       1,
		wantStdout: []string{mod + "/a", mod + "/b"},
		wantStderr: []string{mod + "/b", "80.0%"},
	}, {
		name: "an excluded package is reported and not gated",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 1 1\n" +
			mod + "/a/f.go:6.2,6.11 9 0\n",
		exclusions: mod + "/a  # the encoder needs a device\n",
		want:       0,
		wantStdout: []string{mod + "/a", "10.0%", "not gated: the encoder needs a device"},
	}, {
		name: "an exclusion with no reason is rejected",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 10 1\n",
		exclusions: mod + "/a\n",
		want:       1,
		wantStderr: []string{"has no reason"},
	}, {
		name: "an exclusion with an empty reason is rejected",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 10 1\n",
		exclusions: mod + "/a #   \n",
		want:       1,
		wantStderr: []string{"has no reason"},
	}, {
		// A comment line and a blank line are the file as shipped, so the
		// shipped file must not be an error.
		name: "comments and blank lines in the exclusions file",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 10 1\n",
		exclusions: "# header\n\n   \n",
		want:       0,
	}, {
		// A package with no statements is a package of declarations. Failing
		// it would set a gate no test could ever satisfy.
		name:       "a package with no statements is reported and not failed",
		profile:    "mode: set\n" + mod + "/a/doc.go:1.1,2.2 0 0\n",
		want:       0,
		wantStdout: []string{mod + "/a", "no statements"},
	}, {
		// -coverpkg makes every test binary report every package, so the same
		// block arrives once per binary. Summed, this package would read as
		// 20 statements; merged, it is 10, and the block one binary ran is
		// covered.
		name: "duplicate blocks are merged, not summed",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 10 0\n" +
			mod + "/a/f.go:3.10,5.4 10 3\n",
		want:       0,
		wantStdout: []string{"100.0%", "10/10"},
	}, {
		// A gate that passes on an empty profile is worse than no gate: it is
		// green and it measured nothing.
		name:       "a profile with no blocks fails",
		profile:    "mode: set\n",
		want:       1,
		wantStderr: []string{"holds no coverage blocks"},
	}, {
		name:       "an empty profile fails",
		profile:    "",
		want:       1,
		wantStderr: []string{"holds no coverage blocks"},
	}, {
		name:       "a missing profile fails",
		profile:    "\x00absent",
		want:       1,
		wantStderr: []string{"covercheck:"},
	}, {
		name:       "a profile with no mode line fails",
		profile:    mod + "/a/f.go:3.10,5.4 10 1\n",
		want:       1,
		wantStderr: []string{"mode line"},
	}, {
		name:       "a malformed block fails",
		profile:    "mode: set\n" + mod + "/a/f.go:3.10,5.4 10\n",
		want:       1,
		wantStderr: []string{"malformed profile line"},
	}, {
		name:       "a block with no file fails",
		profile:    "mode: set\n:3.10,5.4 10 1\n",
		want:       1,
		wantStderr: []string{"names no file"},
	}, {
		name:       "a block with no colon fails",
		profile:    "mode: set\n" + mod + "/a/f.go 3.10,5.4 10 1\n",
		want:       1,
		wantStderr: []string{"malformed profile line"},
	}, {
		name:       "a one-sided span fails",
		profile:    "mode: set\n" + mod + "/a/f.go:3.10 10 1\n",
		want:       1,
		wantStderr: []string{"malformed profile line"},
	}, {
		name:       "a position with no column fails",
		profile:    "mode: set\n" + mod + "/a/f.go:3,5.4 10 1\n",
		want:       1,
		wantStderr: []string{"malformed profile line"},
	}, {
		name:       "a position with a non-numeric line fails",
		profile:    "mode: set\n" + mod + "/a/f.go:x.10,5.4 10 1\n",
		want:       1,
		wantStderr: []string{"malformed profile line"},
	}, {
		name:       "a position with a non-numeric column fails",
		profile:    "mode: set\n" + mod + "/a/f.go:3.x,5.4 10 1\n",
		want:       1,
		wantStderr: []string{"malformed profile line"},
	}, {
		// A cancelled test run can truncate a profile into numbers like this,
		// and a negative statement count reports a percentage above 100.
		name:       "a zero line number fails",
		profile:    "mode: set\n" + mod + "/a/f.go:0.10,5.4 10 1\n",
		want:       1,
		wantStderr: []string{"malformed profile line"},
	}, {
		name:       "a negative statement count fails",
		profile:    "mode: set\n" + mod + "/a/f.go:3.10,5.4 -1 1\n",
		want:       1,
		wantStderr: []string{"negative count"},
	}, {
		name:       "a negative execution count fails",
		profile:    "mode: set\n" + mod + "/a/f.go:3.10,5.4 10 -1\n",
		want:       1,
		wantStderr: []string{"negative count"},
	}, {
		name:    "an unknown flag is a usage error, not a failing gate",
		profile: "mode: set\n",
		args:    []string{"-nosuchflag"},
		want:    2,
	}, {
		name: "-min moves the gate",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 5 1\n" +
			mod + "/a/f.go:6.2,6.11 5 0\n",
		args:       []string{"-min", "50"},
		want:       0,
		wantStdout: []string{"at or above 50%"},
	}, {
		name: "-v reports an exclusion that matches nothing",
		profile: "mode: set\n" +
			mod + "/a/f.go:3.10,5.4 10 1\n",
		exclusions: mod + "/gone  # a package that was deleted\n",
		args:       []string{"-v"},
		want:       0,
		wantStdout: []string{"note: " + mod + "/gone is excluded and is not in the profile"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := []string{}
			if c.profile == "\x00absent" {
				args = append(args, "-profile", filepath.Join(t.TempDir(), "nope.out"))
			} else {
				args = append(args, "-profile", write(t, "cover.out", c.profile))
			}
			if c.exclusions != "" {
				args = append(args, "-exclusions", write(t, "exclusions.txt", c.exclusions))
			} else {
				// A path that does not exist is the ordinary case for a tool
				// run from another directory, and it must not weaken or break
				// the gate.
				args = append(args, "-exclusions", filepath.Join(t.TempDir(), "none.txt"))
			}
			args = append(args, c.args...)

			var stdout, stderr bytes.Buffer
			got := run(args, &stdout, &stderr)
			if got != c.want {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", got, c.want, &stdout, &stderr)
			}
			for _, want := range c.wantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout does not carry %q:\n%s", want, &stdout)
				}
			}
			for _, want := range c.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr does not carry %q:\n%s", want, &stderr)
				}
			}
		})
	}
}

// TestReportOrderIsSorted is specs/053-determinism.md applied to this tool. The
// summary groups by package in a map, and a report whose rows move between runs
// cannot be diffed.
func TestReportOrderIsSorted(t *testing.T) {
	profile := "mode: set\n"
	for _, pkg := range []string{"z", "m", "a", "q", "b", "y", "c", "n"} {
		profile += mod + "/" + pkg + "/f.go:3.10,5.4 1 1\n"
	}
	prof := write(t, "cover.out", profile)

	first := ""
	// Ten runs in one process: map iteration order is randomised per range, so
	// a single comparison would pass by luck.
	for i := 0; i < 10; i++ {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-profile", prof, "-exclusions", filepath.Join(t.TempDir(), "none.txt")},
			&stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, &stderr)
		}
		if i == 0 {
			first = stdout.String()
			continue
		}
		if stdout.String() != first {
			t.Fatalf("report differs between runs:\nfirst:\n%s\nrun %d:\n%s", first, i, &stdout)
		}
	}
	if !strings.Contains(first, mod+"/a") || strings.Index(first, mod+"/a") > strings.Index(first, mod+"/z") {
		t.Errorf("the report is not in import path order:\n%s", first)
	}
}

// TestExclusionsFileAsShipped reads the file the gate actually uses, so a typo
// in it fails here and not in CI. It must parse, and it must be empty: an entry
// is a review conversation.
func TestExclusionsFileAsShipped(t *testing.T) {
	got, err := loadExclusions("exclusions.txt")
	if err != nil {
		t.Fatalf("the shipped exclusions file does not parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the shipped exclusions file has %d entries: %v", len(got), got)
	}
}

// TestUnreadableExclusionsFileFails separates "no file" from "a file this
// process may not read". The first is ordinary, the second is a broken
// checkout and must not pass silently.
func TestUnreadableExclusionsFileFails(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root reads a file with no permission bits, so there is nothing to
		// assert. This runs in a container often enough to matter.
		t.Skip("running as root")
	}
	p := write(t, "exclusions.txt", "# nothing\n")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Skipf("cannot remove read permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if _, err := loadExclusions(p); err == nil {
		t.Error("an unreadable exclusions file parsed without error")
	}
}
