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

package chain

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/assemble"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// These are Go tests because a scenario cannot name its own streams, cycles or
// reset records: it derives them from the steps.

// assembled is every node and relation id an assembly of ix holds, with how
// many times each was written, and the follows relations by their two ends.
func assembled(t *testing.T, ix *index.Index) (ids map[string]int, follows [][2]string) {
	t.Helper()
	res, err := assemble.Session(ix, assemble.Options{Conversation: "s", Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ids = map[string]int{}
	for _, n := range res.Nodes {
		ids[n.ID]++
	}
	for _, r := range res.Relations {
		ids[r.ID]++
		if r.Type == model.RelFollows {
			follows = append(follows, [2]string{r.From, r.To})
		}
	}
	return ids, follows
}

// TestIDsReadAsThePageSays. A run names its talk nested in its own id, and so
// does a relation its two ends; a segment and an epoch whose reset record has
// no id name the location where they open. The ids are the ones the Session
// Flow page shows.
func TestIDsReadAsThePageSays(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	main := in.ID("main")
	ix.Append(index.Entry{Seq: 1, Row: 1, Stream: main, Kind: index.KindUser, TS: 1000, Record: in.ID("u1"),
		Run: in.ID("c1"), Flags: index.FlagExternalInput})
	ix.Append(index.Entry{Seq: 1, Row: 2, Stream: main, Kind: index.KindSystem, TS: 1001,
		Parent: in.ID("u1"), Flags: index.FlagEpochBoundary})
	ix.Append(index.Entry{Seq: 1, Row: 3, Stream: main, Kind: index.KindSystem, TS: 1002, Record: in.ID("b1"),
		Parent: in.ID("u1"), Flags: index.FlagEpochBoundary})
	ids, _ := assembled(t, ix)
	for _, id := range []string{"run/talk_main_c1/c1", "segment/at/1/1", "epoch/main/0", "epoch/main/at/1/2",
		"epoch/main/id/b1", "rel/in_segment/talk_main_c1/segment_at_1_1",
		"rel/follows/epoch_main_at_1_2/epoch_main_0", "rel/follows/epoch_main_id_b1/epoch_main_at_1_2"} {
		if ids[id] == 0 {
			t.Errorf("no %s among %v", id, ids)
		}
	}
}

// TestAChildNamedLikeATalkKeepsItsRun. The talk of the main stream's cycle "a"
// is talk/main/a, and the one talk of a child stream named "main_a" is
// talk/main_a. Nested with each "/" written as "_", both read talk_main_a, so
// their runs of the cycle "a" had one id and so did their in_segment
// relations, and one of each took the other's place.
func TestAChildNamedLikeATalkKeepsItsRun(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	main, child := in.ID("main"), in.ID("main_a")
	ix.Append(index.Entry{Seq: 1, Row: 1, Stream: main, Kind: index.KindUser, TS: 1000, Record: in.ID("u1"),
		Run: in.ID("a"), Flags: index.FlagExternalInput})
	ix.Append(index.Entry{Seq: 1, Row: 2, Stream: main, Kind: index.KindAssistant, TS: 1002, Record: in.ID("a1"),
		Parent: in.ID("u1"), Run: in.ID("a")})
	// Inside the main talk's window, so the child's talk is in its segment.
	ix.Append(index.Entry{Seq: 2, Row: 1, Stream: child, Kind: index.KindUser, TS: 1001, Record: in.ID("u2"),
		Run: in.ID("a"), Flags: index.FlagExternalInput})
	ids, _ := assembled(t, ix)
	runs, windows := 0, 0
	for id, n := range ids {
		switch {
		case len(id) > 4 && id[:4] == "run/":
			runs += n
		case len(id) > 16 && id[:16] == "rel/in_segment/t":
			windows += n
		}
	}
	if runs != 2 || windows != 2 {
		t.Fatalf("%d run ids and %d in_segment ids, want 2 of each: %v", runs, windows, ids)
	}
}

// TestResetNamesStayApart. A reset record's own id, the location of one that
// has none, and a stream's first epoch name three kinds of epoch. A reset with
// no id at sequence 1 row 2 and one whose id is "at_1_2" both gave
// epoch/main/at_1_2, and a reset whose id is "0" gave the first epoch's id and
// followed itself.
func TestResetNamesStayApart(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	main := in.ID("main")
	ix.Append(index.Entry{Seq: 1, Row: 1, Stream: main, Kind: index.KindUser, TS: 1000, Record: in.ID("u1"),
		Run: in.ID("c1"), Flags: index.FlagExternalInput})
	ix.Append(index.Entry{Seq: 1, Row: 2, Stream: main, Kind: index.KindSystem, TS: 1001,
		Parent: in.ID("u1"), Flags: index.FlagEpochBoundary})
	ix.Append(index.Entry{Seq: 1, Row: 3, Stream: main, Kind: index.KindSystem, TS: 1002, Record: in.ID("at_1_2"),
		Parent: in.ID("u1"), Flags: index.FlagEpochBoundary})
	ix.Append(index.Entry{Seq: 1, Row: 4, Stream: main, Kind: index.KindSystem, TS: 1003, Record: in.ID("0"),
		Parent: in.ID("u1"), Flags: index.FlagEpochBoundary})
	ids, follows := assembled(t, ix)
	epochs := 0
	for id, n := range ids {
		if len(id) > 6 && id[:6] == "epoch/" {
			epochs += n
		}
	}
	if epochs != 4 || len(follows) != 3 {
		t.Errorf("%d epochs and %d follows relations, want 4 and 3: %v", epochs, len(follows), ids)
	}
	for _, f := range follows {
		if f[0] == f[1] {
			t.Errorf("%s follows itself", f[0])
		}
	}
}
