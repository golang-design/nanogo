// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package hygiene

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The documentation quotes numbers the tests produce. This file is the gate
// that keeps them true.
//
// # Why it exists
//
// Every number in README.md and specs/003-sequencing.md was measured once and
// then copied into prose. Prose does not re-run. An audit in August 2026 found
// the copy had rotted in both directions: specs/003 claimed 4,755 functions
// lowered when the measurement said 8,238, README claimed 41 runtime signatures
// when rtsym checks 45 and 467 packages agreed with `go list` when 520 do, and
// one coverage figure had moved *down* (ssa, 98.1% to 96.6%) with nobody
// noticing. Each of those was correct on the day it was written.
//
// A number that was true once and is wrong now is worse than no number,
// because a reader has no way to tell the two apart.
//
// # Where the truth comes from
//
// Not from re-running the corpus. The corpus tests take about four minutes and
// need a GOROOT with source, so a gate that ran them would be a gate nobody
// runs before pushing.
//
// The truth is a small machine-readable file, [factsPath], holding one number
// per measurement. It is checked in, so the docs-against-facts comparison in
// [TestDocumentedNumbersAreTheMeasuredOnes] runs on a plain `go test` in under
// a second and needs nothing but the repository.
//
// That closes half the loop. The other half is [TestTheFactsAreCurrent], which
// re-derives the facts from the tests themselves and fails when the checked-in
// file has drifted. It is slow, so it runs only where the corpus already runs:
// under NANOGO_REQUIRE_CORPUS=1, which is CI. Local runs prove the docs match
// the facts; CI proves the facts match reality. Without the second half a
// checked-in facts file is only one more number that can go stale.
//
// # What produces the facts file today, and what should produce it
//
// [TestTheFactsAreCurrent] runs the corpus tests as subprocesses and reads the
// counts out of their `t.Logf` lines. That works with no change to any other
// package, which is why it is what exists. It is not the right long-term seam:
// a reworded log line breaks the gate, and the break is reported as a missing
// fact rather than as a reworded log line.
//
// The better seam is for each corpus test to append its own count when an
// environment variable names a file, one JSON object per line:
//
//	{"key":"ssa.corpus.lowered","value":8238}
//
// That is four lines in each corpus test and it removes the scraping. The
// reader here already accepts either form. Until those lines exist, the
// scraper is the producer.

// factsPath is the checked-in measurement file, relative to this directory.
const factsPath = "testdata/facts.json"

// factsBandPercent is how far the line count in specs/000-decisions.md may sit
// from the live measurement before the gate calls it stale.
//
// The line count is the one number that moves on every commit to the compiler,
// so gating it exactly would turn every code change red and the gate would be
// deleted within a week. A band catches the failure that matters, a figure
// that quietly stops describing the tree, and ignores the churn that does not.
// specs/000-decisions.md states the same tolerance so the rule is not hidden
// in a test.
const factsBandPercent = 5.0

// A claimKind says how a documented number is compared with the measured one.
type claimKind int

const (
	// exact requires the documented number to equal the measurement. Used for
	// counts, which move only when the corpus or the toolchain moves.
	exact claimKind = iota
	// floorPercent requires the documented whole percent to equal the
	// measurement rounded down. Coverage is documented as "at least N%"
	// because a gate on the tenth would fire on every added statement.
	floorPercent
)

// A claim is one number the documentation states, and the measurement it must
// agree with.
//
// The pattern must match exactly once in the file. A pattern that matches
// nothing fails the gate rather than skipping it: a reworded sentence is the
// most common way a check like this is silently switched off, and a gate that
// disappears when the prose is edited protects nothing. A pattern that matches
// twice fails too, because then the gate cannot say which number it checked.
type claim struct {
	key     string
	file    string
	pattern *regexp.Regexp
	kind    claimKind
}

// claims is the gated set.
//
// It is deliberately short. Every number in the deck could be gated, and then
// every prose edit would be a build failure and the whole file would be
// deleted. These are the numbers a reader would act on: what the compiler was
// measured against, how far through the distribution it reaches, and how well
// each package is tested.
var claims = buildClaims()

func buildClaims() []claim {
	readme := "README.md"
	seq := "specs/003-sequencing.md"

	var out []claim
	// A prose pattern matches across a line break. Markdown is hard-wrapped,
	// so a sentence that is reflowed by an editor would otherwise stop
	// matching, and the gate would report a reworded claim where nothing was
	// reworded.
	phrase := func(p string) string { return strings.ReplaceAll(p, " ", `\s+`) }
	add := func(key, file, pattern string, kind claimKind) {
		out = append(out, claim{key: key, file: file, pattern: regexp.MustCompile(pattern), kind: kind})
	}
	addPhrase := func(key, file, pattern string) { add(key, file, phrase(pattern), exact) }

	// The differential corpora, in README.md's "What is built" table.
	addPhrase("syntax.scanner.files", readme, `([\d,]+) files agree with `+"`go/scanner`")
	addPhrase("syntax.parser.files", readme, `([\d,]+) agree with `+"`go/parser`")
	addPhrase("loader.constraint.files", readme, `([\d,]+) files on two platforms agree with `+"`go/build`")
	addPhrase("loader.golist.packages", readme, `([\d,]+) packages agree with `+"`go list`")
	addPhrase("arm64.encodings", readme, `([\d,]+) encodings agree with `+"`go tool asm`")
	// The encoding count is written in five places and the gate watched one.
	// When the conditional select class was added it moved to 981,124 in
	// README.md and stayed stale in four specs, which is exactly the drift
	// this gate exists to stop, happening in the documents it was not reading.
	//
	// A number worth gating is worth gating everywhere it is written down.
	addPhrase("arm64.encodings", "specs/README.md", `([\d,]+) encodings agree with `+"`go tool asm`")
	addPhrase("arm64.encodings", "specs/041-instruction-encoding.md", `\*\*([\d,]+) comparisons with zero disagreements\*\*`)
	addPhrase("arm64.encodings", "specs/042-arm64-backend.md", `041-instruction-encoding\.md\): ([\d,]+)`)
	addPhrase("arm64.encodings", "specs/004-conformance.md", `([\d,]+), counted by the package itself`)
	addPhrase("rtsym.symbols", readme, `([\d,]+) runtime signatures checked`)
	addPhrase("types2.subtests", readme, `([\d,]+) subtests`)
	addPhrase("types2.errorcheck.entries", readme, `a ([\d,]+)-entry errorcheck corpus`)
	addPhrase("ssagen.linkandrun.cases", readme, `([\d,]+) programs go from source text`)

	// How far the pipeline reaches. This is the pair of numbers the audit
	// found most wrong, so both files that state it are gated.
	for _, f := range []string{readme, seq} {
		addPhrase("ir.corpus.packages", f, `a typed tree for ([\d,]+) packages`)
		addPhrase("ir.corpus.functions", f, `([\d,]+) functions and [\d,]+ nodes`)
		addPhrase("ir.corpus.nodes", f, `[\d,]+ functions and ([\d,]+) nodes`)
		addPhrase("ssa.corpus.reached", f, `([\d,]+) of those functions reach SSA construction`)
		addPhrase("ssa.corpus.mapped", f, `([\d,]+) of the [\d,]+ carry a stack map`)
	}
	// The same corpus with the lowering pass run first, which is what the
	// driver does. It is a second number and not a correction of the one
	// above: the pair is what says how much of the reach is construction's
	// and how much is specs/020-ir.md's lowering table.
	addPhrase("ir.lowered.reached", readme, `([\d,]+) of them get past construction once the lowering pass has run`)
	// The same pair is written in the spec that owns the seam, and a number
	// worth gating is worth gating everywhere it is written down.
	desc := "specs/032-type-descriptors-and-itabs.md"
	addPhrase("ir.lowered.reached", desc, `([\d,]+) of [\d,]+ distribution functions get past`)
	addPhrase("ir.corpus.functions", desc, `[\d,]+ of ([\d,]+) distribution functions get past`)
	addPhrase("ssa.corpus.reached", desc, `construction with the pass, and ([\d,]+) without it`)

	// Coverage. The package name anchors each row, so a table that is
	// reordered or reformatted still matches and a renamed package does not.
	for _, pkg := range gatedPackages {
		esc := regexp.QuoteMeta(pkg.name)
		add("cover."+pkg.name, readme,
			`\|\s*\[`+"`"+esc+"`"+`\]\([^)]*\)\s*\|\s*(\d+)%`, floorPercent)
		add("cover."+pkg.name, seq,
			`\|\s*`+"`"+esc+"`"+`\s*\|\s*(\d+)%`, floorPercent)
	}
	return out
}

// gatedPackages is every package the coverage gate covers, with the import
// path covercheck reports it under.
//
// The list is written down rather than discovered so that a package which
// stops being tested is a failure here, not a silently shorter table.
var gatedPackages = []struct{ name, importPath string }{
	{"syntax", "golang.design/x/nanogo/syntax"},
	{"loader", "golang.design/x/nanogo/loader"},
	{"ir", "golang.design/x/nanogo/ir"},
	{"export", "golang.design/x/nanogo/export"},
	{"export/pkgbits", "golang.design/x/nanogo/export/pkgbits"},
	{"ssa", "golang.design/x/nanogo/ssa"},
	{"ssa/rules", "golang.design/x/nanogo/ssa/rules"},
	{"ssagen", "golang.design/x/nanogo/ssagen"},
	{"obj", "golang.design/x/nanogo/obj"},
	{"obj/arm64", "golang.design/x/nanogo/obj/arm64"},
	{"rtsym", "golang.design/x/nanogo/rtsym"},
	{"rtype", "golang.design/x/nanogo/rtype"},
	{"driver", "golang.design/x/nanogo/driver"},
}

// TestDocumentedNumbersAreTheMeasuredOnes is the gate itself.
func TestDocumentedNumbersAreTheMeasuredOnes(t *testing.T) {
	skipUnderDerivation(t)
	root := repoRoot(t)
	f := readFacts(t, filepath.Join(root, "internal", "hygiene", factsPath))

	texts := map[string]string{}
	for _, c := range claims {
		if _, ok := texts[c.file]; ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, c.file))
		if err != nil {
			t.Fatalf("read %s: %v", c.file, err)
		}
		texts[c.file] = string(b)
	}

	for _, c := range claims {
		want, ok := f[c.key]
		if !ok {
			t.Errorf("%s claims %s and %s holds no such measurement; "+
				"a claim with nothing behind it is the thing this gate exists to stop",
				c.file, c.key, factsPath)
			continue
		}
		hits := c.pattern.FindAllStringSubmatch(texts[c.file], -1)
		switch {
		case len(hits) == 0:
			t.Errorf("%s no longer says %s in the form this gate reads (%v).\n"+
				"Either restore the wording or update the pattern. A reworded sentence "+
				"must not be a way to switch a check off.", c.file, c.key, c.pattern)
			continue
		case len(hits) > 1:
			t.Errorf("%s states %s %d times, so the gate cannot say which one it checked; "+
				"give the pattern a distinctive anchor", c.file, c.key, len(hits))
			continue
		}
		got, err := parseNumber(hits[0][1])
		if err != nil {
			t.Errorf("%s: %s: %v", c.file, c.key, err)
			continue
		}
		switch c.kind {
		case exact:
			if got != want {
				t.Errorf("%s says %s is %s, the tests measure %s.\n"+
					"Fix the document, then refresh %s with NANOGO_REFRESH_FACTS=1.",
					c.file, c.key, format(got), format(want), factsPath)
			}
		case floorPercent:
			if got != math.Floor(want) {
				t.Errorf("%s says %s is %s%%, the tests measure %.1f%% (floor %.0f%%).\n"+
					"Coverage is documented rounded down, so this number moved.",
					c.file, c.key, format(got), want, math.Floor(want))
			}
		}
	}
}

// TestEveryPathTheDocumentationNamesExists catches the class of drift a
// rename causes.
//
// It is here because it happened: ssa/machop_arm64.go was renamed to
// ssa/macharm64.go, because the old name is a Go build constraint and excluded
// the file from every non-arm64 build, and the specs that cited it kept the
// old name. A path in a spec is a promise that a reader can open the file.
func TestEveryPathTheDocumentationNamesExists(t *testing.T) {
	skipUnderDerivation(t)
	root := repoRoot(t)

	// Two kinds of path are checked, and no others.
	//
	// A markdown link target, because a broken link is a broken document. And
	// a source path written in prose, which must have a directory and a known
	// extension: ssa/macharm64.go is checked and a bare decl.go is not,
	// because a bare name has no directory to resolve against and guessing
	// produces false alarms. A gate with false alarms is a gate nobody keeps.
	link := regexp.MustCompile(`\]\(([^)#\s]+)\)`)
	source := regexp.MustCompile(`\b((?:[\w.-]+/)+[\w.-]+\.(?:go|txt|yml|yaml|s|md))\b`)

	docs := docFiles(t, root)
	if len(docs) < 40 {
		t.Fatalf("only %d documents were scanned, so the scan is broken rather than "+
			"the documentation being clean", len(docs))
	}

	var missing []string
	checked := 0
	for _, doc := range docs {
		b, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		dir := filepath.Dir(doc)
		var candidates []string
		for _, m := range link.FindAllStringSubmatch(string(b), -1) {
			candidates = append(candidates, m[1])
		}
		for _, m := range source.FindAllStringSubmatch(string(b), -1) {
			candidates = append(candidates, m[1])
		}
		for _, p := range candidates {
			p = strings.TrimSuffix(p, "/")
			if !ours(root, p) {
				continue
			}
			checked++
			// A spec cites a sibling spec by bare name and everything else
			// from the repository root, so both are tried.
			if exists(filepath.Join(root, p)) || exists(filepath.Join(root, dir, p)) {
				continue
			}
			missing = append(missing, doc+": "+p)
		}
	}
	if checked < 100 {
		t.Fatalf("only %d paths were checked, so the scan is broken rather than "+
			"the documentation being clean", checked)
	}
	sort.Strings(missing)
	for _, m := range uniq(missing) {
		t.Errorf("%s names a path that is not in the repository; "+
			"a document that cites a file promises the file is there", m)
	}
}

// notOurs names the paths whose first element this repository also has, and
// which nonetheless belong to the Go distribution.
//
// The first-element rule in ours removes runtime/, crypto/ and go/ on its own,
// because this repository has no such directory. It cannot remove
// cmd/compile/... or internal/abi, because cmd/ and internal/ are here too.
var notOurs = []string{
	"cmd/compile", "cmd/link", "cmd/internal", "cmd/go", "cmd/asm", "cmd/api",
	"cmd/dist", "cmd/vendor", "internal/abi", "internal/pkgbits",
	"internal/goarch", "internal/goos", "internal/buildcfg",
}

// ours reports whether a documented path is this repository's to check.
//
// The rule is the first path element. A document names paths in three trees:
// this one, the Go distribution, and the web. Only this one has the first
// element on disk here, so the test needs no list of the other two, only the
// exceptions in notOurs where the two trees share a first element.
func ours(root, p string) bool {
	switch {
	case p == "", strings.HasPrefix(p, "http://"), strings.HasPrefix(p, "https://"),
		strings.HasPrefix(p, "mailto:"), strings.HasPrefix(p, "#"),
		strings.Contains(p, "$"), strings.Contains(p, "*"), strings.Contains(p, "~"):
		return false
	}
	for _, n := range notOurs {
		if p == n || strings.HasPrefix(p, n+"/") {
			return false
		}
	}
	head := p
	for strings.HasPrefix(head, "../") {
		head = head[3:]
	}
	if i := strings.Index(head, "/"); i >= 0 {
		head = head[:i]
	}
	// A spec links a sibling spec by bare name, which resolves against the
	// specs directory.
	if strings.HasSuffix(head, ".md") {
		return true
	}
	return exists(filepath.Join(root, head))
}

// TestTheSpecIndexIsComplete checks the deck's own bookkeeping: frontmatter
// vocabulary, dependencies that resolve, and an index that lists every spec.
//
// specs/README.md is the entry point, so a spec missing from it is a spec
// nobody reads, and a spec listed there that no longer exists is a dead link
// on the first page.
func TestTheSpecIndexIsComplete(t *testing.T) {
	skipUnderDerivation(t)
	root := repoRoot(t)
	dir := filepath.Join(root, "specs")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read specs: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && e.Name() != "README.md" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("no specs were found, so this test proves nothing")
	}

	index, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read specs/README.md: %v", err)
	}
	indexText := string(index)

	// specs/README.md states how many specs the deck holds. The count is in
	// prose, so it goes stale the same way every other number does.
	if m := regexp.MustCompile(`(\d+) specs`).FindStringSubmatch(indexText); m != nil {
		if n, _ := strconv.Atoi(m[1]); n != len(files) {
			t.Errorf("specs/README.md says the deck holds %d specs and it holds %d", n, len(files))
		}
	} else {
		t.Error("specs/README.md no longer states how many specs the deck holds")
	}

	status := regexp.MustCompile(`(?m)^status:\s*(.+?)\s*$`)
	depends := regexp.MustCompile(`(?m)^\s+-\s+(\d\d\d-[\w-]+\.md)\s*$`)
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(b)

		if !strings.Contains(indexText, "]("+name+")") {
			t.Errorf("specs/%s is not linked from specs/README.md, so nothing leads a reader to it", name)
		}
		m := status.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("specs/%s has no status in its frontmatter", name)
			continue
		}
		have := strings.Trim(m[1], `"`)
		switch have {
		case "draft", "in progress", "complete":
		default:
			t.Errorf("specs/%s has status %q; specs/README.md allows draft, in progress and complete, "+
				"and a fourth value means the index cannot be read mechanically", name, m[1])
		}
		// The index repeats the status so that the deck can be read at a
		// glance. Two copies of a fact drift, so the copy is gated.
		row := regexp.MustCompile(`\]\(` + regexp.QuoteMeta(name) + `\)[^|\n]*\|[^|\n]*\|\s*` + "`" + `([^` + "`" + `]+)` + "`")
		if r := row.FindStringSubmatch(indexText); r == nil {
			t.Errorf("specs/README.md has no status cell for %s", name)
		} else if r[1] != have {
			t.Errorf("specs/README.md says %s is %q and its frontmatter says %q", name, r[1], have)
		}
		for _, d := range depends.FindAllStringSubmatch(text, -1) {
			if !exists(filepath.Join(dir, d[1])) {
				t.Errorf("specs/%s depends on %s, which is not in the deck", name, d[1])
			}
		}
	}

	// The other direction: a link in the index to a spec that was deleted.
	for _, m := range regexp.MustCompile(`\]\((\d\d\d-[\w-]+\.md)\)`).FindAllStringSubmatch(indexText, -1) {
		if !exists(filepath.Join(dir, m[1])) {
			t.Errorf("specs/README.md links to %s, which is not in the deck", m[1])
		}
	}
}

// TestTheFactsAreCurrent re-derives every measurement and fails when the
// checked-in file has drifted from it.
//
// This is the half of the loop that cannot be fast. It runs the corpus, so it
// runs where the corpus runs: under NANOGO_REQUIRE_CORPUS=1. With
// NANOGO_REFRESH_FACTS=1 it writes the file instead of failing, which is how
// the file is regenerated after the toolchain or the compiler moves.
func TestTheFactsAreCurrent(t *testing.T) {
	skipUnderDerivation(t)
	refresh := os.Getenv("NANOGO_REFRESH_FACTS") == "1"
	// Deriving the facts re-runs every corpus, which takes minutes and blows
	// the default go test timeout when the whole suite runs behind it. Under
	// -short the documented numbers are still checked against the recorded
	// facts by TestTheDocumentsAgreeWithTheFacts; what is skipped is only the
	// re-derivation, which is the slow half.
	if testing.Short() && !refresh {
		t.Skip("-short: the facts are checked against the documents, not re-derived")
	}
	if os.Getenv("NANOGO_REQUIRE_CORPUS") != "1" && !refresh {
		t.Skip("NANOGO_REQUIRE_CORPUS is not set, so the facts are taken on trust; " +
			"CI sets it and re-derives them")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	root := repoRoot(t)

	got := deriveFacts(t, root)
	path := filepath.Join(root, "internal", "hygiene", factsPath)
	if refresh {
		writeFacts(t, path, got)
		t.Logf("wrote %d measurements to %s", len(got), factsPath)
		return
	}

	have := readFacts(t, path)

	// A corpus whose size depends on what is on the machine cannot be a fixed
	// fact. The scanner, the parser and the constraint evaluator all read every
	// Go source tree they can find, so a developer with a checkout of the Go
	// repository beside GOROOT compares far more files than a CI runner with
	// GOROOT alone. Pinning those counts makes the gate fail for everyone whose
	// machine differs from whoever refreshed it last, which is not drift.
	//
	// They are still checked, in the direction that means something: the
	// documented number must be reachable, so the measurement may not exceed
	// it, and it may not collapse to nothing.
	// Every count derived from a corpus is elastic, because the corpus is
	// whatever this machine and this platform hold.
	//
	// Two things move it, and both are legitimate. The source trees present
	// differ: a developer with a checkout of the Go repository beside GOROOT
	// compares far more files than a runner with GOROOT alone. And the
	// packages that build differ by platform: 536 build on darwin and 477 on
	// linux, because a build constraint excludes the rest, so every number
	// downstream of "which packages built" moves with the operating system.
	//
	// Pinning any of them makes the gate fail for everyone whose machine is
	// not the one that refreshed it last, which is not drift. They are checked
	// in the direction that means something instead: below, see the switch.
	elastic := map[string]bool{
		"syntax.scanner.files":    true,
		"syntax.parser.files":     true,
		"loader.constraint.files": true,
		"loader.golist.packages":  true,
		"ir.corpus.functions":     true,
		"ir.corpus.nodes":         true,
		"ir.corpus.packages":      true,
		"ssa.corpus.reached":      true,
		"ssa.corpus.lowered":      true,
		"ssa.corpus.mapped":       true,
		"ssa.corpus.functions":    true,
		"ir.lowered.reached":      true,
		// The link tests run only where nanogo's output can run, which is an
		// arm64 host, so this is zero on the amd64 runner by design.
		"ssagen.linkandrun.cases": true,
	}

	// Coverage is elastic downward for a different reason. nanogo emits arm64,
	// so the tests that link and run its output skip on any other host, and a
	// coverage number measured there is a number for a smaller suite. The
	// documented figure is what the complete suite produces, so a measurement
	// below it is a host that ran less, and one above it means the documents
	// undersell the work.
	coverage := func(k string) bool { return strings.HasPrefix(k, "cover.") }

	// A measurement of zero is normally the corpus having gone missing, which
	// is the failure worth catching. For the counts that a whole platform
	// skips by design, zero is the correct answer there and says nothing about
	// this machine.
	zeroIsLegitimate := map[string]bool{
		"ssagen.linkandrun.cases": true,
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		old, ok := have[k]
		switch {
		case !ok:
			t.Errorf("%s holds no %s and the tests produce %s", factsPath, k, format(got[k]))
		case coverage(k):
			if got[k] > old {
				t.Errorf("%s says %s is %s and the tests produce %s, which is more.\n"+
					"Refresh it with NANOGO_REFRESH_FACTS=1 so the documents do not undersell it.",
					factsPath, k, format(old), format(got[k]))
			}
		case elastic[k]:
			// The corpus is whatever this machine and this platform hold, so a
			// count that differs is information about the host rather than
			// drift.
			//
			// The rule here used to be "a count above the documented one means
			// the document undersells the work", and that was wrong for a
			// count whose direction is not a quality. linux resolves 525
			// packages where darwin resolves 521, because a build constraint
			// includes a different set, and neither number is the better one.
			// Requiring the documented figure to be the maximum made the
			// gate fail on whichever platform happened to have more.
			//
			// What is left is the check that catches a real fault: a
			// measurement of zero means the corpus went missing and the test
			// proved nothing, whatever the platform.
			if got[k] == 0 && !zeroIsLegitimate[k] {
				t.Errorf("%s measured nothing; the corpus is missing and the test proved nothing", k)
			}
		case old != got[k]:
			t.Errorf("%s says %s is %s and the tests produce %s.\n"+
				"Refresh it with NANOGO_REFRESH_FACTS=1 and correct whatever document quotes it.",
				factsPath, k, format(old), format(got[k]))
		}
	}
	for k := range have {
		if _, ok := got[k]; !ok {
			t.Errorf("%s holds %s and no test produces it any more; "+
				"a measurement with no measurement behind it is a number nobody can check", factsPath, k)
		}
	}
}

// deriveFacts runs the tests that produce a documented number and reads the
// counts out of their output.
//
// Scraping a log line is the producer that needs no change to any other
// package. The doc comment at the top of this file says what should replace
// it. A pattern that stops matching is reported by the caller as a missing
// fact, which is loud, and that is the property that matters most.
func deriveFacts(t *testing.T, root string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}

	type scrape struct {
		key     string
		pattern *regexp.Regexp
	}
	runs := []struct {
		what string
		args []string
		// corpus says whether this run needs NANOGO_REQUIRE_CORPUS. It is set
		// per run rather than for all of them because ssagen's TestMain fails
		// when the variable is set and the -run filter left its differential
		// tests out, which is exactly what a narrow run here does.
		corpus   bool
		scrapes  []scrape
		coverage bool
	}{
		{
			what:   "the corpus counts",
			corpus: true,
			args: []string{"test", "-count=1", "-v", "-run", "Corpus",
				"./ssa/", "./loader/", "./syntax/", "./ir/"},
			scrapes: []scrape{
				{"syntax.scanner.files", regexp.MustCompile(`compared (\d+) of \d+ files`)},
				{"syntax.parser.files", regexp.MustCompile(`compared (\d+) files from`)},
				{"loader.constraint.files", regexp.MustCompile(`darwin/arm64: (\d+) files, \d+ with an error`)},
				{"ir.corpus.packages", regexp.MustCompile(`built (\d+) packages of \d+, \d+ functions`)},
				{"ir.corpus.functions", regexp.MustCompile(`built \d+ packages of \d+, (\d+) functions`)},
				{"ir.corpus.nodes", regexp.MustCompile(`built \d+ packages of \d+, \d+ functions, (\d+) nodes`)},
				{"ssa.corpus.reached", regexp.MustCompile(`(\d+) functions reached SSA`)},
				{"ssa.corpus.lowered", regexp.MustCompile(`functions reached SSA, (\d+) lowered completely`)},
				{"ssa.corpus.mapped", regexp.MustCompile(`\d+ lowered, (\d+) mapped`)},
				{"ir.lowered.reached", regexp.MustCompile(`functions, (\d+) past construction with the pass`)},
			},
		},
		{
			what:    "the go list agreement",
			corpus:  true,
			args:    []string{"test", "-count=1", "-v", "-run", "TestGoListDifferential", "./loader/"},
			scrapes: []scrape{{"loader.golist.packages", nil}}, // summed below
		},
		{
			what:    "the encoder",
			corpus:  true,
			args:    []string{"test", "-count=1", "-v", "./obj/arm64/"},
			scrapes: []scrape{{"arm64.encodings", nil}}, // summed below
		},
		{
			what:   "the runtime symbols",
			corpus: true,
			args:   []string{"test", "-count=1", "-v", "-run", "TestTableMatchesTheRuntime", "./rtsym/"},
			scrapes: []scrape{
				{"rtsym.symbols", regexp.MustCompile(`checked (\d+) symbols against`)},
			},
		},
		{
			what:    "the link and run cases",
			args:    []string{"test", "-count=1", "-v", "-run", "TestLinkAndRun", "./ssagen/"},
			scrapes: []scrape{{"ssagen.linkandrun.cases", nil}}, // counted below
		},
		{
			what:    "the type checker",
			args:    []string{"test", "-count=1", "-v", "./types2/"},
			scrapes: []scrape{{"types2.subtests", nil}, {"types2.errorcheck.entries", nil}},
		},
		{
			what:     "coverage",
			coverage: true,
		},
	}

	for _, r := range runs {
		if r.coverage {
			for k, v := range coverageFacts(t, root) {
				out[k] = v
			}
			continue
		}
		log := goTest(t, root, r.corpus, r.args...)
		switch r.what {
		case "the go list agreement":
			// The test walks two patterns, the standard library and this
			// module, and logs one line each. README states the total.
			sum := 0.0
			for _, m := range regexp.MustCompile(`: (\d+) packages, \d+ files`).FindAllStringSubmatch(log, -1) {
				n, _ := strconv.Atoi(m[1])
				sum += float64(n)
			}
			out["loader.golist.packages"] = sum
		case "the encoder":
			// The package counts its own comparisons in an atomic and prints
			// the total from TestMain. Summing the per-test log lines instead
			// gives a smaller number, because not every comparison is made
			// inside a test that logs one, and it also double counts this
			// line. An audit did exactly that and put 913,069 into two specs
			// where the package itself says 963,460.
			m := regexp.MustCompile(`arm64: (\d+) encodings compared against go tool asm`).FindStringSubmatch(log)
			if m == nil {
				t.Error("obj/arm64 printed no total; TestMain's summary line is what this reads")
				break
			}
			n, _ := strconv.Atoi(m[1])
			out["arm64.encodings"] = float64(n)
		case "the link and run cases":
			out["ssagen.linkandrun.cases"] = float64(len(
				regexp.MustCompile(`(?m)^\s+--- (?:PASS|SKIP|FAIL): TestLinkAndRun/`).FindAllString(log, -1)))
		case "the type checker":
			// Passing results only, top level and subtest, which is the 613
			// the deck has quoted since the port landed. Counting skips too
			// gives 624 and counting only indented lines gives 485. Passing
			// is the number worth gating: a test that starts skipping stops
			// proving anything, and the gate should say so.
			out["types2.subtests"] = float64(len(
				regexp.MustCompile(`(?m)^\s*--- PASS: `).FindAllString(log, -1)))
			n := 0
			for _, top := range []string{"TestCheck", "TestSpec", "TestExamples", "TestFixedbugs", "TestLocal"} {
				n += len(regexp.MustCompile(`(?m)^\s+--- (?:PASS|SKIP|FAIL): `+top+`/`).FindAllString(log, -1))
			}
			out["types2.errorcheck.entries"] = float64(n)
		default:
			for _, s := range r.scrapes {
				m := s.pattern.FindStringSubmatch(log)
				if m == nil {
					t.Errorf("%s produced no %s; the log line this gate reads was reworded, "+
						"so update the pattern %v", r.what, s.key, s.pattern)
					continue
				}
				n, _ := strconv.Atoi(m[1])
				out[s.key] = float64(n)
			}
		}
	}
	return out
}

// coverageFacts runs the coverage gate the way CONTRIBUTING.md documents it
// and reads the per-package number out of covercheck's report.
//
// covercheck prints a percentage as %6.1f, so one decimal is the whole of the
// measurement and [format] stores it without loss. If it ever prints more, the
// stored value would be a truncation of the derived one and every run after
// would report a difference that no refresh could settle.
//
// NANOGO_COVERPROFILE names a profile that already exists. Setting it in the
// CI job that produces one removes the full coverage run this otherwise does
// for itself, which is the expensive half of a facts refresh.
func coverageFacts(t *testing.T, root string) map[string]float64 {
	t.Helper()
	profile := os.Getenv("NANOGO_COVERPROFILE")
	if profile == "" {
		profile = filepath.Join(t.TempDir(), "cover.out")
		goTest(t, root, true, "test", "-count=1",
			"-coverprofile="+profile, "-coverpkg=./...", "./...")
	}
	cmd := exec.Command("go", "run", "./internal/covercheck", "-profile="+profile)
	cmd.Dir = root
	cmd.Env = childEnv(t.TempDir())
	// The exit status is not checked. covercheck exits non-zero when a
	// package is below the gate, and that is a report this wants to read
	// rather than a reason to stop: the number is still measured, and a
	// documentation gate that refused to run while a package was untested
	// would go dark exactly when the tree is moving.
	b, _ := cmd.CombinedOutput()
	out := map[string]float64{}
	for _, p := range gatedPackages {
		re := regexp.MustCompile(regexp.QuoteMeta(p.importPath) + `\s+(\d+\.\d+)%`)
		m := re.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("covercheck reported nothing for %s; the package is in the gated list "+
				"and has no coverage, which is a hole rather than a formatting change", p.importPath)
			continue
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		out["cover."+p.name] = v
	}
	return out
}

// childTimeout is the timeout given to every go test this file spawns.
//
// A child gets the 10 minute default unless it is told otherwise, and the
// derivation's children run whole corpora: the four corpus packages together
// passed 10 minutes once the lowering pass reached more functions, and the
// coverage child runs ./... . The failure is a test binary panicking on its
// own alarm, which reads as a compiler bug rather than as a budget.
//
// The comment on TestTheFactsAreCurrent already recorded that the derivation
// "takes minutes and blows the default go test timeout". That was true of the
// child as well, and only the parent was given a way out, through -short. So
// the gate was flaky by construction and CI's -timeout 40m could not save it,
// because a timeout on the parent is not inherited.
const childTimeout = "40m"

func goTest(t *testing.T, root string, corpus bool, args ...string) string {
	t.Helper()
	// After "test", so that it is a go test flag and not a go flag.
	if len(args) > 0 && args[0] == "test" {
		args = append([]string{"test", "-timeout", childTimeout}, args[1:]...)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	env := childEnv(t.TempDir())
	if corpus {
		env = append(env, "NANOGO_REQUIRE_CORPUS=1")
	} else {
		env = append(env, "NANOGO_REQUIRE_CORPUS=")
	}
	cmd.Env = env
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, lastLines(string(b), 40))
	}
	return string(b)
}

// skipUnderDerivation skips a gate that is running inside the derivation's own
// subprocess.
//
// The derivation runs `go test ./...` to produce a coverage profile, and that
// runs this package again. The gates cannot pass there: the file they read is
// the file being written, and a failure would stop the run that writes it.
// Measuring and checking are separate jobs, and the child is only measuring.
func skipUnderDerivation(t *testing.T) {
	t.Helper()
	if os.Getenv(factsChildEnv) == "1" {
		t.Skip("this process is measuring for a facts refresh, not checking")
	}
}

// factsChildEnv marks a process started by the derivation.
//
// The coverage step runs `go test ./...`, which runs this package's tests
// again. Without the mark the child would derive its own facts, and so would
// its children.
const factsChildEnv = "NANOGO_FACTS_CHILD"

// childEnv is this process's environment, with the refresh switch removed, the
// recursion mark set, and TMPDIR pointed at a directory this test owns.
//
// The derivation starts a whole `go test ./...` and that tree starts compilers
// of its own, some of them under a context with a deadline. A process killed
// on its deadline does not run its deferred cleanup, so the scratch directory
// it made under TMPDIR outlives it, and the parent is the only thing left that
// can remove it. Pointing the child tree at a directory this test removes
// contains every such leak whatever caused it, rather than chasing whichever
// compile happened to be too slow on the day.
//
// This is not hypothetical and it is not the child's fault. CI reported two
// nanogo-build directories surviving a run, on the arm64 runner only, where
// the derivation runs the suite again under coverage instrumentation and every
// compile is slower.
func childEnv(tmp string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "NANOGO_REFRESH_FACTS=") ||
			strings.HasPrefix(kv, factsChildEnv+"=") ||
			strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "NANOGO_REFRESH_FACTS=", factsChildEnv+"=1", "TMPDIR="+tmp)
}

func readFacts(t *testing.T, path string) map[string]float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\nRegenerate it with NANOGO_REFRESH_FACTS=1 go test ./internal/hygiene/", path, err)
	}
	out := map[string]float64{}
	// The file is one JSON object. A corpus test that appends its own count
	// writes one JSON object per line instead, so both forms are read: the
	// producer can change without the gate changing with it.
	if err := json.Unmarshal(b, &out); err == nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Key   string  `json:"key"`
			Value float64 `json:"value"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out[rec.Key] = rec.Value
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no measurements", path)
	}
	return out
}

func writeFacts(t *testing.T, path string, f map[string]float64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range f {
		keys = append(keys, k)
	}
	// Sorted, because specs/053-determinism.md's rule applies to a file this
	// repository writes as much as to an object file: a map range would
	// reorder the file on every refresh and every diff would be noise.
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{\n")
	for i, k := range keys {
		fmt.Fprintf(&b, "  %q: %s", k, format(f[k]))
		if i != len(keys)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// format prints a measurement the way the file stores it: an integer count
// without a decimal point, a percentage with one.
func format(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// parseNumber reads a number as the documentation writes it, with thousands
// separators.
func parseNumber(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

func documentedNumber(t *testing.T, text, pattern, what string) float64 {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("specs/000-decisions.md no longer states %s in the form this gate reads (%s)", what, pattern)
	}
	v, err := parseNumber(m[1])
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return v
}

// docFiles is every document this repository writes, relative to the root.
func docFiles(t *testing.T, root string) []string {
	t.Helper()
	out := []string{"README.md", "CONTRIBUTING.md"}
	entries, err := os.ReadDir(filepath.Join(root, "specs"))
	if err != nil {
		t.Fatalf("read specs: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, "specs/"+e.Name())
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func uniq(s []string) []string {
	var out []string
	var last string
	for i, v := range s {
		if i == 0 || v != last {
			out = append(out, v)
		}
		last = v
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestChildEnvOwnsTheChildTreesTemporaryDirectory pins the containment.
//
// The derivation starts a `go test ./...` that starts compilers of its own,
// some under a context with a deadline. A process killed on its deadline does
// not run its deferred cleanup, so whatever it made under TMPDIR outlives it
// and only the parent can remove it. The child tree therefore has to write
// into a directory this package owns.
//
// The old environment is dropped rather than appended to, because a variable
// set twice is read by the last one on some systems and the first on others,
// and a containment that depends on that is not a containment.
func TestChildEnvOwnsTheChildTreesTemporaryDirectory(t *testing.T) {
	// Taken before TMPDIR is overridden, because t.TempDir reads it.
	dir := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(dir, "not-this-one"))
	t.Setenv("NANOGO_REFRESH_FACTS", "1")
	var tmpdirs, refresh, marks []string
	for _, kv := range childEnv(dir) {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "TMPDIR":
			tmpdirs = append(tmpdirs, v)
		case "NANOGO_REFRESH_FACTS":
			refresh = append(refresh, v)
		case factsChildEnv:
			marks = append(marks, v)
		}
	}
	if len(tmpdirs) != 1 || tmpdirs[0] != dir {
		t.Errorf("TMPDIR is %v, want exactly one entry naming %q", tmpdirs, dir)
	}
	// The refresh switch has to be off in the child or the child refreshes the
	// facts it was started to measure, and the mark has to be on or the child
	// starts a derivation of its own, and so does its child.
	if len(refresh) != 1 || refresh[0] != "" {
		t.Errorf("NANOGO_REFRESH_FACTS is %v, want exactly one empty entry", refresh)
	}
	if len(marks) != 1 || marks[0] != "1" {
		t.Errorf("%s is %v, want exactly one entry set to 1", factsChildEnv, marks)
	}
}
