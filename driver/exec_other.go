// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

//go:build !unix

package driver

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// execTool runs the real tool as a child, for platforms with no exec.
func execTool(path string, args []string) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 1, fmt.Errorf("%s: %v", path, err)
	}
	return 0, nil
}
