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

package chain_test

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Kill the real writer after it has written part of a round. Returning an
// error would run its deferred cleanup and cannot reproduce a process crash.
func TestKilledRoundWriterDoesNotAdvanceTheChain(t *testing.T) {
	const childPath = "ASZ_TEST_INTERRUPTED_ROUND"
	if path := os.Getenv(childPath); path != "" {
		err := storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
			if _, err := io.WriteString(w, "incomplete round\n"); err != nil {
				return err
			}
			if _, err := io.WriteString(os.Stdout, "written\n"); err != nil {
				return err
			}
			_, err := io.Copy(io.Discard, os.Stdin)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	s := newStage(t, growing)
	s.through("one")
	first := s.parse()
	chain := sessionflow.OpenChain(s.zone.Root(), s.session)
	path := filepath.Join(chain.RoundsDir(), "r000002-000000000000.sf")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKilledRoundWriterDoesNotAdvanceTheChain$")
	cmd.Env = append(os.Environ(), childPath+"="+path)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "written\n" {
		t.Fatalf("writer did not reach its partial write: %q, %v", line, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial round became visible before publication: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("writer exited normally; the crash was not exercised")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("killed writer left a published round: %v", err)
	}
	if head, err := chain.Head(); err != nil || head != first.Number {
		t.Fatalf("killed writer advanced the chain: head %d, %v", head, err)
	}

	// A new parser must ignore the abandoned temporary file and publish the
	// next complete round from the landed evidence.
	s.through("two")
	next := s.parse()
	if next.Number != first.Number+1 || next.FromSeq != first.ThroughSeq+1 {
		t.Fatalf("restart wrote round %d from seq %d, want round %d from %d",
			next.Number, next.FromSeq, first.Number+1, first.ThroughSeq+1)
	}
	if files, err := chain.Verify(); err != nil || len(files) != 2 {
		t.Fatalf("chain after restart: %d rounds, %v", len(files), err)
	}
}
