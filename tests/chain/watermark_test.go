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
)

// A round narrowed by the byte budget reads nothing past its watermark, not
// even through a lookup. A reset at sequence 1 names the record it continues
// from, and that record lands only at sequence 2. Assembled through sequence 1,
// the round must report the record as missing: nothing it covers holds it, and
// its header and input digest cover sequence 1 alone.
func TestARoundReadsNothingPastItsWatermark(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	main := in.ID("main")
	ix.Append(index.Entry{Seq: 1, Row: 1, Stream: main, Kind: index.KindUser, TS: 1000, Record: in.ID("u1"),
		Flags: index.FlagExternalInput})
	ix.Append(index.Entry{Seq: 1, Row: 2, Stream: main, Kind: index.KindSystem, TS: 1001, Record: in.ID("b1"),
		Parent: in.ID("u1"), Continues: in.ID("x1"), Flags: index.FlagEpochBoundary})
	ix.Append(index.Entry{Seq: 2, Row: 1, Stream: main, Kind: index.KindUser, TS: 1002, Record: in.ID("x1"),
		Parent: in.ID("b1")})

	open := func(through uint64) bool {
		res, err := assemble.Session(ix, assemble.Options{Conversation: "s", Session: "s", ThroughSeq: through})
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range res.Unresolved {
			if u.Kind == "epoch_continuation" && u.RefID == "x1" {
				return true
			}
		}
		return false
	}
	if !open(1) {
		t.Error("through sequence 1, the continuation landed at sequence 2 was found")
	}
	if open(2) {
		t.Error("through sequence 2, the continuation is reported missing")
	}
	if _, ok := ix.EntryByRecord("x1"); !ok {
		t.Error("narrowing a round changed the index it was given")
	}
}

// The header counts Bash runs by the tool and agent call nodes the round
// holds. Two tool uses whose ids give one node id are one node, so they count
// once, as the fold shows them.
func TestBashRunsCountTheNodes(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	main := in.ID("main")
	for row, id := range []string{"a/b", "a_b"} {
		ix.Append(index.Entry{Seq: 1, Row: uint32(2*row + 1), Stream: main, Kind: index.KindAssistant,
			TS: int64(1000 + 2*row), Record: in.ID("u" + id), Call: in.ID("m" + id)},
			index.Block{Kind: index.BlockToolUse, ToolID: in.ID(id), Name: in.ID("Bash")})
		ix.Append(index.Entry{Seq: 1, Row: uint32(2*row + 2), Stream: main, Kind: index.KindUser,
			TS: int64(1001 + 2*row), Record: in.ID("r" + id)},
			index.Block{Kind: index.BlockToolResult, ToolID: in.ID(id)})
	}
	res, err := assemble.Session(ix, assemble.Options{Conversation: "s", Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	nodes := 0
	for _, n := range res.Nodes {
		if n.Kind == "tool" {
			nodes++
		}
	}
	if nodes != 1 || res.ToolCalls["Bash"] != nodes {
		t.Fatalf("%d tool nodes, and Bash counted %d times", nodes, res.ToolCalls["Bash"])
	}
}
