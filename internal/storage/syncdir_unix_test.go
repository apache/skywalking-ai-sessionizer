// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

//go:build !windows

package storage

import (
	"fmt"
	"syscall"
	"testing"
)

// A filesystem that cannot sync a directory says so, and that is not a
// failure; an error of the disk is.
func TestAnUnsupportedSyncIsNotAFailure(t *testing.T) {
	// ENOTSUP and EOPNOTSUPP are one number on Linux, so this is a list.
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{syscall.EINVAL, true}, {syscall.ENOTSUP, true}, {syscall.EOPNOTSUPP, true},
		{syscall.ENOSYS, true}, {syscall.ENOTTY, true}, {fmt.Errorf("sync: %w", syscall.EINVAL), true},
		{syscall.EIO, false}, {syscall.ENOSPC, false},
	} {
		if got := unsupported(tc.err); got != tc.want {
			t.Errorf("%v: unsupported %v, want %v", tc.err, got, tc.want)
		}
	}
	if err := syncDirEntries(t.TempDir()); err != nil {
		t.Fatalf("a real directory: %v", err)
	}
}
