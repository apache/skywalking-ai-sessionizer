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

//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
)

// TestNoArgumentWithAnEventOnASocketIsAHook: Claude Code 2.1.260 on macOS
// handed every hook its event on a socket, not on a pipe. A check for a
// pipe alone missed it, so the binary printed the usage text, exited 2,
// and Claude Code blocked the tool.
func TestNoArgumentWithAnEventOnASocketIsAHook(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, b := os.NewFile(uintptr(fds[0]), "event"), os.NewFile(uintptr(fds[1]), "claude")
	defer a.Close()
	defer b.Close()
	if got := subcommand(nil, a); got != "hook" {
		t.Fatalf("no argument and a socket: %q, want hook", got)
	}
}
