// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

// Command nanogo-dist builds a nanogo distribution and says what is in one.
//
//	nanogo-dist build -version nanogo0.1.0 -out dist    build the tree and the tarball
//	nanogo-dist tally [root]                            what compiled each archive
//	nanogo-dist verify [root]                           check VERSION against the archives
//
// It ships inside the tarball as bin/nanogo-dist, so that a tree can be asked
// about itself with nothing else installed. That is the point of it: the
// tarball's archives are gc's work today, and a downloaded distribution that
// could not say so would be exactly the fault specs/054-distribution.md exists
// to prevent.
//
// With no root, tally and verify read the tree the running binary is installed
// in, one directory above bin. That is the same rule driver.FindRoot uses and
// it is written out here rather than imported, because dist must not depend on
// driver.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"golang.design/x/nanogo/dist"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
	nanogo-dist build [flags]    build a distribution tree and its tarball
	nanogo-dist tally [root]     report what compiled each archive in a tree
	nanogo-dist verify [root]    check a tree's VERSION against its archives
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "build":
		err = build(args[1:], stdout)
	case "tally":
		err = tally(args[1:], stdout)
	case "verify":
		err = verify(args[1:], stdout)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "nanogo-dist: unknown command %q\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "nanogo-dist: %v\n", err)
		return 1
	}
	return 0
}

// defaultTarget is the one target nanogo has. It is the host's, so that a
// developer on another machine gets an error naming their platform rather than
// a tree of archives that cannot run.
func defaultTarget() string { return runtime.GOOS + "_" + runtime.GOARCH }

func build(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stdout)
	var (
		release = fs.String("version", "", "the nanogo release, as nanogo0.1.0")
		pin     = fs.String("go", "", "the Go release the tree is pinned to, as go1.27.0")
		goroot  = fs.String("goroot", "", "the toolchain to copy the standard library from")
		goCmd   = fs.String("gocmd", "go", "the go command to resolve the bootstrap closure with")
		target  = fs.String("target", defaultTarget(), "the GOOS_GOARCH the archives are for")
		binary  = fs.String("binary", "", "the nanogo command to install as bin/nanogo")
		self    = fs.String("self", "", "the nanogo-dist command to install as bin/nanogo-dist")
		license = fs.String("license", "LICENSE", "nanogo's own licence")
		out     = fs.String("out", "", "the directory to build the tree in; it must not exist")
		tarball = fs.String("tarball", "", "the tarball to write; empty writes none")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || *binary == "" {
		return fmt.Errorf("build needs -out and -binary")
	}
	if *goroot == "" {
		return fmt.Errorf("build needs -goroot, the toolchain the sources are copied from")
	}
	goos, goarch, _ := cutTarget(*target)
	pkgs, err := dist.Closure(*goCmd, goos, goarch)
	if err != nil {
		return err
	}
	commands := map[string]string{}
	if *self != "" {
		commands["nanogo-dist"] = *self
	}
	v, err := dist.Build(dist.Options{
		Release:   *release,
		GoVersion: *pin,
		Target:    *target,
		GOROOT:    *goroot,
		Binary:    *binary,
		Commands:  commands,
		License:   *license,
		Packages:  pkgs,
		Out:       *out,
	})
	if err != nil {
		return err
	}
	line, err := dist.TallyLine(*out, *target)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, line)
	if *tarball != "" {
		name, err := dist.TarballName(v.Release, v.Target)
		if err != nil {
			return err
		}
		// The tarball always unpacks to a directory called nanogo, whatever
		// the tree was built in, the way Go's unpacks to go.
		if err := writeTarball(*tarball, *out, dist.TreeName); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "nanogo-dist: wrote %s as %s\n", name, *tarball)
	}
	return nil
}

// writeTarball packs the tree at root under prefix.
func writeTarball(name, root, prefix string) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	if err := dist.WriteTarGz(f, root, prefix); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func tally(args []string, stdout io.Writer) error {
	root, err := rootOf(args)
	if err != nil {
		return err
	}
	v, err := dist.ReadVersion(root)
	if err != nil {
		return err
	}
	line, err := dist.TallyLine(root, v.Target)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, line)
	return nil
}

func verify(args []string, stdout io.Writer) error {
	root, err := rootOf(args)
	if err != nil {
		return err
	}
	if err := dist.VerifyTree(root); err != nil {
		return err
	}
	v, err := dist.ReadVersion(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "nanogo-dist: %s in %s agrees with its %d archives\n", v.Release, root, v.Packages)
	return nil
}

// rootOf is the tree to read: the argument, or the tree the running binary is
// installed in.
func rootOf(args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("one root at a time, and %d were given", len(args))
	}
	if len(args) == 1 {
		return args[0], nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(filepath.Dir(exe)), nil
}

// cutTarget splits GOOS_GOARCH. An invalid target is reported by dist.Build,
// which is the one place that has to reject it.
func cutTarget(target string) (goos, goarch string, ok bool) {
	for i := 0; i < len(target); i++ {
		if target[i] == '_' {
			return target[:i], target[i+1:], true
		}
	}
	return target, "", false
}
