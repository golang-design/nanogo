// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package export

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.design/x/nanogo/obj"
	"golang.design/x/nanogo/types2"
)

// TestGcReadsWhatNanogoWrote is the cross-read, and it is the test the format
// stands or falls on.
//
// A round trip through nanogo's own reader agrees with itself, and would go on
// agreeing with itself about a format that is wrong. Here gc reads the bytes:
// for each package, nanogo writes the export data into an archive of its own
// and the installed toolchain compiles a file that names every exported
// declaration of it. That runs both of gc's readers over nanogo's bytes, the
// types2 one that resolves each name and the backend one that turns each into
// a symbol.
//
// internal/e2e carries the other half of the claim, where the program links
// and runs. This one is wide rather than deep: it covers every package of the
// standard library under NANOGO_REQUIRE_CORPUS and [stdlib] without it.
func TestGcReadsWhatNanogoWrote(t *testing.T) {
	goCmd := goTool(t)
	tc, err := obj.VerifyToolchain()
	if err != nil {
		t.Skipf("cannot probe the installed toolchain: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module nanogo.example/xread\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := stdlib
	if os.Getenv("NANOGO_REQUIRE_CORPUS") == "1" {
		want = []string{"std"}
	}
	list := archives(t, dir, want...)
	if len(list) == 0 {
		t.Fatal("no archive was found, so the test proved nothing")
	}

	r := NewReader()
	total, read, refused := len(list), 0, 0
	var refusals []string
	for _, a := range list {
		path, file := a[0], a[1]
		pkg, err := r.Read(path, file)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		resolve(t, pkg)

		payload, _, err := Write(pkg, false)
		if err != nil {
			if u, ok := err.(*UnsupportedError); ok {
				refused++
				refusals = append(refusals, u.Package+": "+u.Name)
				continue
			}
			t.Errorf("%s: Write: %v", path, err)
			continue
		}

		if out, err := gcCompilesAnImporter(t, goCmd, tc.Header, dir, path, pkg, payload); err != nil {
			t.Errorf("%s: gc cannot read what nanogo wrote: %v\n%s", path, err, out)
			continue
		}
		read++
	}

	t.Logf("gc read %d of %d standard library packages nanogo wrote; %d were refused", read, total, refused)
	for _, name := range refusals {
		t.Logf("refused %s", name)
	}
	if read == 0 {
		t.Fatal("gc read nothing, so the test proved nothing")
	}
}

// gcCompilesAnImporter asks the installed toolchain to compile a file that
// names every exported declaration of pkg, against an archive that carries
// nothing but the export data nanogo wrote.
//
// The archive has one member, because internal/exportdata reads __.PKGDEF and
// a compile does not look at the object. Naming every declaration is what
// forces the whole type graph: gc's reader is lazy, so an importer that names
// nothing decodes nothing.
func gcCompilesAnImporter(t *testing.T, goCmd, header, dir, path string, pkg *types2.Package, payload []byte) (string, error) {
	t.Helper()
	work := t.TempDir()

	def, err := Definition(header, false, payload)
	if err != nil {
		return "", err
	}
	archive := filepath.Join(work, "p.a")
	var b []byte
	b = append(b, archiveMagic...)
	b = append(b, fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", definitionMember, 0, 0, 0, 0o644, len(def))...)
	b = append(b, def...)
	if len(def)%2 != 0 {
		b = append(b, 0)
	}
	if err := os.WriteFile(archive, b, 0o600); err != nil {
		return "", err
	}

	var src strings.Builder
	fmt.Fprintf(&src, "package importer\n\nimport p %q\n\n", path)
	n := 0
	for _, name := range pkg.Scope().Names() {
		o := pkg.Scope().Lookup(name)
		if o == nil || !o.Exported() {
			continue
		}
		// A type is named as the type of a variable and everything else as
		// its value, because those are the two ways a declaration can be
		// mentioned and each forces a different part of the decoder. A
		// constant is redeclared as a constant so that it keeps its own kind:
		// assigning math.MaxUint to a variable makes it an int and overflows.
		switch o.(type) {
		case *types2.TypeName:
			fmt.Fprintf(&src, "var _%d p.%s\n", n, name)
		case *types2.Const:
			fmt.Fprintf(&src, "const _%d = p.%s\n", n, name)
		default:
			fmt.Fprintf(&src, "var _%d = p.%s\n", n, name)
		}
		n++
	}
	if n == 0 {
		// A package with no exported declaration is still worth compiling:
		// the import alone makes gc walk the whole object list.
		src.Reset()
		fmt.Fprintf(&src, "package importer\n\nimport _ %q\n", path)
	}

	file := filepath.Join(work, "importer.go")
	if err := os.WriteFile(file, []byte(src.String()), 0o600); err != nil {
		return "", err
	}
	cfg := filepath.Join(work, "importcfg")
	if err := os.WriteFile(cfg, []byte("packagefile "+path+"="+archive+"\n"), 0o600); err != nil {
		return "", err
	}

	cmd := exec.Command(goCmd, "tool", "compile", "-p", "nanogo.example/xread/importer",
		"-o", filepath.Join(work, "importer.o"), "-importcfg", cfg, file)
	cmd.Dir = dir
	cmd.Env = env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
