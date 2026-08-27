// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package link

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// loadProgram reads every archive the program links and builds the global
// symbol table, in the order the linker uses.
func loadProgram(t *testing.T, b *build) *Loader {
	t.Helper()
	archive := map[string]string{}
	for _, pf := range b.packagefiles(t) {
		archive[pf[0]] = pf[1]
	}
	l := NewLoader()
	err := l.LoadProgram(b.pkg, func(pkg string) ([]byte, string, error) {
		file, ok := archive[pkg]
		if !ok {
			return nil, "", fmt.Errorf("the import configuration does not name it")
		}
		data, err := os.ReadFile(file)
		return data, file, err
	})
	if err != nil {
		t.Fatalf("building the symbol table: %v", err)
	}
	if d := l.Duplicates(); len(d) > 0 {
		t.Errorf("the loader could not merge %d definitions, the first is %s", len(d), d[0])
	}
	return l
}

// dumpdep runs the real linker on the same archives and returns the set of
// symbol names its reachability pass reached.
//
// The output is one line per edge, "from -> to", where a name carries an
// attribute suffix when the linker set one. The suffix is stripped here
// and checked separately, and "_" is the root and not a symbol.
func dumpdep(t *testing.T, b *build) (names map[string]bool, attrs map[string]string) {
	t.Helper()
	goCmd := goTool(t)
	main := ""
	for _, pf := range b.packagefiles(t) {
		if pf[0] == b.pkg {
			main = pf[1]
		}
	}
	if main == "" {
		t.Fatal("the import configuration does not name the main package")
	}
	cmd := exec.Command(goCmd, "tool", "link", "-dumpdep",
		"-importcfg", b.cfg, "-o", filepath.Join(t.TempDir(), "a.out"), main)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool link -dumpdep: %v\n%s", err, head(out))
	}
	names = map[string]bool{}
	attrs = map[string]string{}
	edges := 0
	for _, line := range strings.Split(string(out), "\n") {
		from, to, ok := strings.Cut(line, " -> ")
		if !ok {
			continue
		}
		edges++
		for _, side := range []string{from, to} {
			name, attr := splitAttr(side)
			if name == "_" || name == "" {
				continue
			}
			names[name] = true
			if attr != "" {
				attrs[name] = attr
			}
		}
	}
	if edges == 0 {
		t.Fatalf("the linker printed no dependency edge:\n%s", head(out))
	}
	t.Logf("cmd/link -dumpdep: %d edges over %d symbols", edges, len(names))
	return names, attrs
}

// splitAttr separates a name from the attributes -dumpdep appends to it.
//
// The attributes are a space and then one or both of two fixed words. They
// are stripped as a suffix and not found by searching, because a symbol
// name holds spaces and angle brackets of its own: go:string." < " and the
// name of a struct type with a channel field both do.
func splitAttr(s string) (name, attr string) {
	for {
		switch {
		case strings.HasSuffix(s, "<ReflectMethod>"):
			s, attr = strings.TrimSuffix(s, "<ReflectMethod>"), "<ReflectMethod>"+attr
		case strings.HasSuffix(s, "<UsedInIface>"):
			s, attr = strings.TrimSuffix(s, "<UsedInIface>"), "<UsedInIface>"+attr
		default:
			if attr != "" {
				s = strings.TrimSuffix(s, " ")
			}
			return s, attr
		}
	}
}

func head(b []byte) []byte {
	if len(b) > 4000 {
		return b[:4000]
	}
	return b
}

// keptWithNoEdge is the set of symbols cmd/link keeps by setting the
// attribute rather than by marking, so that -dumpdep prints no line for
// them and the dump is short by one name.
//
// There is one. The map initialiser cleanup redirects the pruned
// initialiser of a map nothing reads to runtime.mapinitnoop and keeps
// that function, and it does so after the flood, with no edge. The dump
// is therefore not the whole reachable set, and
// TestReachableTextMatchesTheBinary reads the set cmd/link actually
// wrote out of the executable it produced.
var keptWithNoEdge = map[string]bool{"runtime.mapinitnoop": true}

// TestReachableTextMatchesTheBinary is the second reachability oracle,
// and it sees what cmd/link -dumpdep cannot.
//
// The dump is a list of edges, so a symbol cmd/link keeps without an edge
// is missing from it. The symbol table of the executable cmd/link wrote
// from the same archives has no such gap for text: every function in the
// binary is a function the linker kept.
func TestReachableTextMatchesTheBinary(t *testing.T) {
	for _, b := range []*build{&hostBuild, &reflectBuild} {
		t.Run(b.pkg, func(t *testing.T) {
			b := b.get(t)
			want := nmText(t, linkExe(t, b))

			l := loadProgram(t, b)
			l.InitTasks()
			r := l.Deadcode(runtime.GOOS, runtime.GOARCH)
			mine, defined := map[string]bool{}, map[string]bool{}
			for g := Global(1); g < Global(l.NSym()); g++ {
				s := l.Def(g)
				if s == nil || !s.Type.IsText() {
					continue
				}
				name := l.symtabName(g)
				if name == "" {
					continue
				}
				defined[name] = true
				if r.Reachable(g) {
					mine[name] = true
				}
			}

			var missing []string
			for name := range want {
				// A function in the binary that no object defines is one
				// the linker made, and the stage that makes those is the
				// layout and not this one.
				if _, ok := nmMatch(defined, name); !ok {
					continue
				}
				if _, ok := nmMatch(mine, name); !ok {
					missing = append(missing, name)
				}
			}
			sort.Strings(missing)
			t.Logf("the binary holds %d text symbols, %d of them defined by an object, and nanogo reaches %d functions",
				len(want), len(defined), len(mine))
			if len(missing) > 0 {
				t.Errorf("the binary holds %d functions nanogo does not reach, the first are %v",
					len(missing), first(missing, 20))
			}
		})
	}
}

// TestPrunedMapInitIsRewritten checks the two things the map initialiser
// cleanup does, on a program that has one.
//
// The host build reaches time, whose package initialiser holds a weak
// call to the initialiser of a map nothing in this program reads. The
// call is rewritten, so runtime.mapinitnoop is kept and time.init is
// recorded as rewritten. The second half is what the layout stage reads:
// cmd/link rewrites a symbol by copying it out of its object, and its
// text order then lays the copy out after the package's own text.
func TestPrunedMapInitIsRewritten(t *testing.T) {
	b := hostBuild.get(t)
	l := loadProgram(t, b)
	l.InitTasks()
	r := l.Deadcode(runtime.GOOS, runtime.GOARCH)

	init := l.Lookup("time.init", VerABIInternal)
	if init == 0 {
		t.Fatal("the program does not link time, so it cannot show this")
	}
	if mapInit := l.Lookup("time.map.init.0", VerABIInternal); mapInit == 0 {
		t.Fatal("time has no map initialiser, so the weak call this is about is not there")
	} else if r.Reachable(mapInit) {
		t.Fatal("the program reads the map, so its initialiser is not pruned and this proves nothing")
	}
	if !r.Rewritten(init) {
		t.Error("time.init holds a weak call to a pruned map initialiser and was not recorded as rewritten")
	}
	noop := l.Lookup("runtime.mapinitnoop", VerABIInternal)
	if noop == 0 || !r.Reachable(noop) {
		t.Error("the call was redirected to runtime.mapinitnoop and the function was not kept")
	}
}

// TestReachabilityAgreesWithTheLinker is the oracle specs/045-linker.md
// names for the second stage: the set nanogo keeps against the set
// cmd/link -dumpdep reports for the same program.
//
// The comparison is over the symbol set and not the edge list. Both
// implementations walk the same graph, and the edge that first reaches a
// symbol depends on the order a work queue pops, which is not a fact about
// the program.
func TestReachabilityAgreesWithTheLinker(t *testing.T) {
	for _, b := range []*build{&hostBuild, &reflectBuild} {
		t.Run(b.pkg, func(t *testing.T) {
			b := b.get(t)
			want, attrs := dumpdep(t, b)

			l := loadProgram(t, b)
			l.InitTasks()
			d := l.Deadcode(runtime.GOOS, runtime.GOARCH)
			got := d.Names()
			t.Logf("nanogo: %d reachable symbols, %d of them named", d.Count(), len(got))

			var missing, surplus []string
			for name := range want {
				if !got[name] {
					missing = append(missing, name)
				}
			}
			for name := range got {
				if !want[name] && !keptWithNoEdge[name] {
					surplus = append(surplus, name)
				}
			}
			sort.Strings(missing)
			sort.Strings(surplus)
			t.Logf("%d symbols cmd/link keeps and nanogo drops, %d nanogo keeps and cmd/link drops",
				len(missing), len(surplus))
			if len(missing) > 0 {
				t.Errorf("cmd/link keeps %d symbols nanogo drops, the first are %v", len(missing), first(missing, 20))
			}
			if len(surplus) > 0 {
				t.Errorf("nanogo keeps %d symbols cmd/link drops, the first are %v", len(surplus), first(surplus, 20))
			}

			// The attribute the dump prints beside a name is the second
			// check. A type that reached an interface decides which
			// methods survive, so the two passes agreeing on the set and
			// disagreeing on the attribute would be an agreement by
			// accident.
			checkAttributes(t, l, d, attrs)
		})
	}
}

// checkAttributes compares the interface and reflection attributes the
// dump prints with the ones the pass computed.
func checkAttributes(t *testing.T, l *Loader, d *Reachability, attrs map[string]string) {
	t.Helper()
	mine := map[string]string{}
	for g := Global(1); g < Global(l.NSym()); g++ {
		if !d.Reachable(g) {
			continue
		}
		name := l.Name(g)
		if name == "" {
			continue
		}
		var a string
		if l.UsedInIface(g) {
			a += "<UsedInIface>"
		}
		if s := l.Def(g); s != nil && s.ReflectMethod() {
			a += "<ReflectMethod>"
		}
		if a != "" {
			mine[name] = a
		}
	}
	var wrong []string
	for name, a := range attrs {
		if mine[name] != a {
			wrong = append(wrong, name+": cmd/link says "+a+" and nanogo says "+quoteAttr(mine[name]))
		}
	}
	for name, a := range mine {
		if attrs[name] == "" {
			wrong = append(wrong, name+": nanogo says "+a+" and cmd/link says none")
		}
	}
	sort.Strings(wrong)
	t.Logf("%d symbols carry an attribute, %d disagree", len(attrs), len(wrong))
	if len(wrong) > 0 {
		t.Errorf("%d attributes disagree, the first are %v", len(wrong), first(wrong, 10))
	}
}

func quoteAttr(a string) string {
	if a == "" {
		return "none"
	}
	return a
}

func first(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// TestInitTasksAreARoot checks the list the linker synthesises before
// reachability runs.
//
// Without it the whole initialisation chain is unreachable, and the
// reachable set is short by most of the program. The check is that the
// list exists, that it names the records the program has to run, and that
// every one of them is reachable.
func TestInitTasksAreARoot(t *testing.T) {
	b := reflectBuild.get(t)
	l := loadProgram(t, b)
	l.InitTasks()

	tasks := l.MainInitTasks()
	if tasks == 0 {
		t.Fatal("the linker built no initialisation task list")
	}
	if got := l.Name(tasks); got != "go:main.inittasks" {
		t.Errorf("the list is named %q, and cmd/link names it go:main.inittasks", got)
	}
	d := l.Deadcode(runtime.GOOS, runtime.GOARCH)
	if !d.Reachable(tasks) {
		t.Fatal("the initialisation task list is not reachable, and it is a root")
	}
	// Every record the list names must be reachable, and a package with an
	// initialiser must be among them.
	syn := l.synthetic(tasks)
	if len(syn.targets) == 0 {
		t.Fatal("the list is empty, and the program initialises several packages")
	}
	found := false
	for _, g := range syn.targets {
		if !d.Reachable(g) {
			t.Errorf("%s is in the list and is not reachable", l.Name(g))
		}
		if l.Name(g) == "os..inittask" {
			found = true
		}
	}
	if !found {
		t.Errorf("the list of %d records does not name os..inittask, and the program imports os", len(syn.targets))
	}
	// A record that has nothing to run orders the others and is not in the
	// list the runtime walks.
	for _, g := range syn.targets {
		if s := l.Def(g); s != nil && s.Size <= initTaskEntrySize {
			t.Errorf("%s has no functions to run and is in the list", l.Name(g))
		}
	}
	t.Logf("the list holds %d of the program's initialisation records", len(syn.targets))

	// The runtime keeps its own list, in a slice header the linker
	// rewrites. Nothing in any object holds that edge.
	sh := l.Lookup("runtime.runtime_inittasks", VerABI0)
	if sh == 0 {
		t.Fatal("the runtime does not declare runtime_inittasks")
	}
	if len(l.extra[sh]) != 1 || l.Name(l.extra[sh][0]) != "go:runtime.inittasks" {
		t.Errorf("the slice header points at %v", l.extra[sh])
	}
	if !d.Reachable(l.extra[sh][0]) {
		t.Error("the runtime's own initialisation list is not reachable")
	}
}

// TestInitTasksWithNoRoot checks the case where the program has no
// initialisation record at all.
func TestInitTasksWithNoRoot(t *testing.T) {
	l := NewLoader()
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	l.InitTasks()
	if g := l.MainInitTasks(); g != 0 {
		t.Errorf("a program with no initialisation record has a list at %d", g)
	}
}

// TestReachedByNamesTheEdge checks the diagnostic that turns a
// disagreement about one symbol into a chain that names its cause.
func TestReachedByNamesTheEdge(t *testing.T) {
	b := hostBuild.get(t)
	l := loadProgram(t, b)
	l.InitTasks()
	d := l.Deadcode(runtime.GOOS, runtime.GOARCH)

	entry := l.Lookup(EntrySymbol(runtime.GOOS, runtime.GOARCH), VerABI0)
	if entry == 0 {
		t.Fatalf("the program has no %s", EntrySymbol(runtime.GOOS, runtime.GOARCH))
	}
	if got := d.ReachedBy(entry); got != 0 {
		t.Errorf("the entry point was reached by %s, and it is a root", l.Name(got))
	}
	main := l.Lookup("main.main", VerABIInternal)
	if main == 0 || !d.Reachable(main) {
		t.Fatal("main.main is not reachable")
	}
	// Every symbol but a root has a parent that is itself reachable, and
	// the chain from any symbol ends at a root.
	steps := 0
	for g := main; g != 0; g = d.ReachedBy(g) {
		if !d.Reachable(g) {
			t.Fatalf("%s is on the chain from main.main and is not reachable", l.Name(g))
		}
		steps++
		if steps > 1000 {
			t.Fatal("the chain from main.main does not reach a root")
		}
	}
	if steps < 2 {
		t.Errorf("the chain from main.main is %d symbols long", steps)
	}
	if d.ReachedBy(0) != 0 || d.ReachedBy(Global(l.NSym())) != 0 {
		t.Error("a symbol outside the table was reached by something")
	}
	if d.Reachable(0) || d.Reachable(-1) || d.Reachable(Global(l.NSym())) {
		t.Error("a symbol outside the table is reachable")
	}
}
