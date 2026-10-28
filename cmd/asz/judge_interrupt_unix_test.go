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
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
)

// A CLI may end the navigation server with a signal while its input is still
// open, as Claude Code 2.1.312 does with SIGINT. The server still closes its
// log with every answer it gave, so the run takes its receipts.
func TestAnInterruptedServerClosesItsLog(t *testing.T) {
	if args := os.Getenv("ASZ_NAV_SERVER_ARGS"); args != "" {
		os.Exit(cmdJudgeMCP(strings.Split(args, "\n")))
	}
	root, session, doc := servedFixture(t)
	dir := t.TempDir()
	task, log := filepath.Join(dir, "task.json"), filepath.Join(dir, "mcp-calls.jsonl")
	attempt := "j000001/5-divergence/c001/a1/test"
	b, _ := json.Marshal(judge.Assignment{Attempt: attempt})
	if err := os.WriteFile(task, b, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-root", root, "-conversation", session, "-round", "1", "-digest", doc.Head.Digest, "-task", task, "-log", log}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		_ = os.Remove(log)
		cmd := exec.Command(os.Args[0], "-test.run=^TestAnInterruptedServerClosesItsLog$")
		cmd.Env = append(os.Environ(), "ASZ_NAV_SERVER_ARGS="+strings.Join(args, "\n"))
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"talks","arguments":{}}}`+"\n")
		reply, err := bufio.NewReaderSize(out, 1<<20).ReadString('\n')
		if err != nil || !strings.Contains(reply, `"id":1`) {
			_ = cmd.Process.Kill()
			t.Fatalf("%s: answered %q, %v", sig, reply, err)
		}
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		ended := make(chan error, 1)
		go func() { ended <- cmd.Wait() }()
		select {
		case err := <-ended:
			if err != nil {
				t.Fatalf("%s: the server ended with %v", sig, err)
			}
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%s: the server did not end", sig)
		}
		nav := judge.NewNav(doc, judge.Compute(doc, "claude-code/1", nil, nil), nil)
		nav.TakeLog(log, attempt)
		if g := nav.GivenWindow(); g.Counts == nil || g.Counts.Listed == 0 {
			written, _ := os.ReadFile(log)
			t.Fatalf("%s: %+v\nlog:\n%s", sig, g, written)
		}
	}
}
