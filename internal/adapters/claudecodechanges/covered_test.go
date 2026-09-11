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

package claudecodechanges_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Covered holds exactly when a pass would land nothing from a session's
// change files. A stream file with no record yet is 0 bytes, has nothing to
// land, and is covered.
func TestCoveredMeansAPassWouldLandNothing(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(src, "asz-changes-inline", "output", session)
	main := filepath.Join(out, "main.jsonl")
	appendTo(t, main, line("p1/c1", "main", "toolu_1", "/w"))
	appendTo(t, filepath.Join(out, agent+".jsonl"), "")
	zone := storage.NewZone(t.TempDir())
	c := claudecodechanges.New(src, zone, 0)
	covered := func() (bool, string) {
		t.Helper()
		sessions, err := claudecodechanges.Discover(src)
		if err != nil || len(sessions) != 1 {
			t.Fatalf("discovered %d sessions, want 1: %v", len(sessions), err)
		}
		ok, reason, err := c.Covered(sessions[0])
		if err != nil {
			t.Fatalf("Covered: %v", err)
		}
		return ok, reason
	}
	collect := func() {
		t.Helper()
		st, err := c.CollectAll(nil)
		if err != nil || len(st.Errors) != 0 {
			t.Fatalf("collect: %v %v", err, st.Errors)
		}
	}

	if ok, reason := covered(); ok || !strings.Contains(reason, "main.jsonl is not landed to its end yet") {
		t.Fatalf("before any pass: covered=%v reason=%q", ok, reason)
	}
	if _, err := os.Stat(zone.SessionDir(session)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Covered created the session's directory: %v", err)
	}
	collect()
	if ok, reason := covered(); !ok {
		t.Fatalf("after a pass: %s", reason)
	}

	// A record written in two parts. A pass leaves the first part behind, so
	// the session is covered only once a pass lands the whole line.
	rec := line("p1/c2", "main", "toolu_2", "/w")
	appendTo(t, main, rec[:20])
	if ok, _ := covered(); ok {
		t.Fatal("a line still being written reads as landed")
	}
	collect()
	if ok, _ := covered(); ok {
		t.Fatal("after a pass, a line still being written reads as landed")
	}
	appendTo(t, main, rec[20:])
	if ok, _ := covered(); ok {
		t.Fatal("a finished line no pass has taken reads as landed")
	}
	collect()
	if ok, reason := covered(); !ok {
		t.Fatalf("after the line landed: %s", reason)
	}
}

// Two plugin directories can each hold a file for one stream. A pass would
// interleave them behind one cursor, so neither is covered.
func TestCoveredRefusesTwoFilesForOneStream(t *testing.T) {
	src := t.TempDir()
	appendTo(t, filepath.Join(src, "asz-changes-inline", "output", session, "main.jsonl"), line("p1/c1", "main", "toolu_1", "/w"))
	appendTo(t, filepath.Join(src, "asz-changes-market", "output", session, "main.jsonl"), line("p2/c1", "main", "toolu_1", "/w"))
	sessions, err := claudecodechanges.Discover(src)
	if err != nil || len(sessions) != 1 || len(sessions[0].Sources) != 2 {
		t.Fatalf("discover: %+v err=%v", sessions, err)
	}
	c := claudecodechanges.New(src, storage.NewZone(t.TempDir()), 0)
	if ok, _, err := c.Covered(sessions[0]); ok || err == nil || !strings.Contains(err.Error(), "both map") {
		t.Fatalf("two files for one stream: covered=%v err=%v", ok, err)
	}
}
