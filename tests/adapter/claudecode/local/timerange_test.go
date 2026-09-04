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

package local_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestRoundsCarryTheRecordTimeRange checks the two time pairs a chain
// carries: each round's header holds the range of the files it consumed,
// and the session node holds the range of the whole conversation so far,
// which only ever grows. Both come from record times the runtime wrote, so
// they are part of the round and never from the parser's clock.
func TestRoundsCarryTheRecordTimeRange(t *testing.T) {
	src := t.TempDir()
	z := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)
	opts := parse.Options{Conversation: growSession, Session: growSession, Reindex: index.Rebuild}
	collect := func() {
		t.Helper()
		if _, err := claudecode.New(src, z, 0).CollectAll(nil); err != nil {
			t.Fatal(err)
		}
	}
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("time %q is not RFC 3339: %v", s, err)
		}
		return v
	}
	sessionRange := func(v *sessionflow.View) (string, string) {
		t.Helper()
		n := v.Nodes[sessionflow.NodeID("session", growSession)]
		if n == nil {
			t.Fatal("no session node")
		}
		var a map[string]any
		if err := json.Unmarshal(n.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		from, _ := a["from_time"].(string)
		through, _ := a["through_time"].(string)
		return from, through
	}

	x.AddTurn("first", false)
	x.Flush()
	collect()
	if _, err := parse.Session(z, opts); err != nil {
		t.Fatal(err)
	}
	v1, err := parse.View(z.Root(), growSession)
	if err != nil {
		t.Fatal(err)
	}
	if v1.FromTime == "" || v1.ThroughTime == "" || at(v1.ThroughTime).Before(at(v1.FromTime)) {
		t.Fatalf("round 1 window: %q..%q", v1.FromTime, v1.ThroughTime)
	}
	if from, through := sessionRange(v1); from != v1.FromTime || through != v1.ThroughTime {
		t.Fatalf("after one round the session range must be the window: session %q..%q, window %q..%q", from, through, v1.FromTime, v1.ThroughTime)
	}
	if from, through := sessionRange(v1); v1.SessionFromTime != from || v1.SessionThroughTime != through {
		t.Fatalf("the header must repeat the session node's range: header %q..%q, node %q..%q", v1.SessionFromTime, v1.SessionThroughTime, from, through)
	}

	x.AddTurn("second", false)
	x.Flush()
	collect()
	if _, err := parse.Session(z, opts); err != nil {
		t.Fatal(err)
	}
	v2, err := parse.View(z.Root(), growSession)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Round != 2 || v2.FromTime == "" || v2.ThroughTime == "" {
		t.Fatalf("round 2 window: round=%d %q..%q", v2.Round, v2.FromTime, v2.ThroughTime)
	}
	from, through := sessionRange(v2)
	if at(from).After(at(v1.FromTime)) || at(from).After(at(v2.FromTime)) {
		t.Fatalf("the session begin must not move later: %q after round 1 %q / round 2 %q", from, v1.FromTime, v2.FromTime)
	}
	if at(through).Before(at(v1.ThroughTime)) || at(through).Before(at(v2.ThroughTime)) {
		t.Fatalf("the session end must cover both windows: %q, round 1 %q / round 2 %q", through, v1.ThroughTime, v2.ThroughTime)
	}
	if from != v1.FromTime {
		t.Fatalf("the session began at %q in round 1 and reads %q after round 2", v1.FromTime, from)
	}
	if v2.SessionFromTime != from || v2.SessionThroughTime != through {
		t.Fatalf("round 2 header session range %q..%q, node %q..%q", v2.SessionFromTime, v2.SessionThroughTime, from, through)
	}
}
