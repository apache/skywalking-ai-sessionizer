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

package claudecode_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
)

func TestInspectRetentionSkipsNamedPipes(t *testing.T) {
	if root := os.Getenv("ASZ_TEST_RETENTION_PIPE_ROOT"); root != "" {
		got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
		checkRetentionProblem(t, got, os.Getenv("ASZ_TEST_RETENTION_PIPE_PATH"), "skipped settings path because it is not a regular file")
		return
	}
	for _, path := range []string{"user/settings.json", "managed/managed-settings.json", "managed/managed-settings.d/a.json"} {
		for _, link := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/link=%t", path, link), func(t *testing.T) {
				root := t.TempDir()
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				pipe := full
				if link {
					pipe = filepath.Join(root, "named-pipe")
				}
				if err := syscall.Mkfifo(pipe, 0o600); err != nil {
					t.Fatal(err)
				}
				if link {
					if err := os.Symlink(pipe, full); err != nil {
						t.Fatal(err)
					}
				}
				// A child process lets the test stop a blocked pipe read.
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInspectRetentionSkipsNamedPipes$")
				cmd.Env = append(os.Environ(), "ASZ_TEST_RETENTION_PIPE_ROOT="+root, "ASZ_TEST_RETENTION_PIPE_PATH="+full)
				output, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("inspection blocked on named pipe %q: %v\n%s", full, ctx.Err(), output)
				}
				if err != nil {
					t.Fatalf("inspection failed: %v\n%s", err, output)
				}
			})
		}
	}
}
