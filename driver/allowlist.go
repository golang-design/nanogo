// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package driver

import (
	"fmt"
	"os"
	"strings"
)

// AllowlistEnv names the file that lists the packages nanogo compiles. Every
// other package goes to gc.
//
// The list is the project's progress metric, per
// specs/051-build-integration.md: its length is how much of Go nanogo
// compiles, and the smallest package not on it is what comes next.
const AllowlistEnv = "NANOGO_ALLOWLIST"

// Allowlist is a set of package import paths. The zero value and a nil
// Allowlist hold nothing, so an unset AllowlistEnv sends every package to gc.
type Allowlist struct {
	pkgs map[string]bool
}

// Has reports whether nanogo compiles the named package.
func (a *Allowlist) Has(path string) bool {
	if a == nil || path == "" {
		return false
	}
	return a.pkgs[path]
}

// Len is the number of packages on the list.
func (a *Allowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.pkgs)
}

// ParseAllowlist reads one import path per line. A # starts a comment, and
// blank lines are ignored.
func ParseAllowlist(data []byte) *Allowlist {
	a := &Allowlist{pkgs: make(map[string]bool)}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		a.pkgs[line] = true
	}
	return a
}

// LoadAllowlist reads the allowlist from a file.
func LoadAllowlist(name string) (*Allowlist, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", AllowlistEnv, err)
	}
	return ParseAllowlist(data), nil
}

// AllowlistFromEnv reads the allowlist named by AllowlistEnv.
//
// An unset variable gives an empty list, which is the safe state: nanogo
// compiles nothing and every package goes to gc. A variable that names a file
// nanogo cannot read is an error and not an empty list, because a typed path
// would otherwise turn nanogo off for ever without saying so.
func AllowlistFromEnv(getenv func(string) string) (*Allowlist, error) {
	name := getenv(AllowlistEnv)
	if name == "" {
		return &Allowlist{}, nil
	}
	return LoadAllowlist(name)
}
