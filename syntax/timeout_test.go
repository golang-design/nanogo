// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package syntax

import "time"

// timeout returns a channel that fires after long enough for any terminating
// traversal in these tests to have finished.
func timeout() <-chan time.Time { return time.After(5 * time.Second) }
