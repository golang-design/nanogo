// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

//go:build unix

package driver

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// execTool replaces the nanogo process image with the real tool.
//
// A wrapper that forked and waited would have to translate the child's exit
// status back into its own, and a translation cannot reproduce a death by
// signal: the parent would report an ordinary non-zero exit. Replacing the
// image removes the translation. The go command then observes the real tool's
// exit status, its signal disposition and its standard streams exactly, which
// is what the passthrough in spikes/toolexec exists to prove.
//
// It returns only when the tool cannot start.
func execTool(path string, args []string) (int, error) {
	bin, err := exec.LookPath(path)
	if err != nil {
		return 1, fmt.Errorf("%s: %v", path, err)
	}
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, path)
	argv = append(argv, args...)
	err = syscall.Exec(bin, argv, os.Environ())
	return 1, fmt.Errorf("%s: %v", path, err)
}
