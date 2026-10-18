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

package sessionflow_test

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestDifferentPartsGiveDifferentIDs. A runtime's id can hold the separator,
// and the escape character too. The tool ids "a/b" and "a_b" gave one node
// id, so assembly kept one of the two calls and lost the other.
func TestDifferentPartsGiveDifferentIDs(t *testing.T) {
	seen := map[string][]string{}
	for _, parts := range [][]string{
		{"a/b"}, {"a_b"}, {"a", "b"}, {"a%2Fb"}, {"a%b"}, {"a%25b"}, {"a%"}, {"a%25"},
	} {
		id := sessionflow.NodeID("tool", parts...)
		if prev, dup := seen[id]; dup {
			t.Errorf("parts %q and %q both give %s", prev, parts, id)
		}
		seen[id] = parts
	}
	// Nested in another id, two different ids stay apart too: the talk of the
	// main stream's cycle "a" and the talk of a child stream named "main_a",
	// two tools whose ids differ in a "/" and a "_", and ids that differ only
	// in where a "_" stands.
	for _, pair := range [][2]string{
		{"talk/main/a", "talk/main_a"},
		{sessionflow.NodeID("tool", "a/b"), sessionflow.NodeID("tool", "a_b")},
		{"talk/a_b/c", "talk/a/b_c"},
		{"epoch/main/at/1/2", "epoch/main/at_1_2"},
	} {
		if id := sessionflow.RelationID("starts", pair[0], "stream/c"); id == sessionflow.RelationID("starts", pair[1], "stream/c") {
			t.Errorf("the relations from %s and from %s are both %s", pair[0], pair[1], id)
		}
		if id := sessionflow.RelationID("starts", "stream/c", pair[0]); id == sessionflow.RelationID("starts", "stream/c", pair[1]) {
			t.Errorf("the relations to %s and to %s are both %s", pair[0], pair[1], id)
		}
		if id := sessionflow.NodeIDIn("run", pair[0], "a"); id == sessionflow.NodeIDIn("run", pair[1], "a") {
			t.Errorf("the runs in %s and in %s are both %s", pair[0], pair[1], id)
		}
		// Nested again, and once more. The runs in talk/main/a and in
		// talk/main_a are run/talk_main_a/a and run/talk_main%5Fa/a, and a
		// nested id that kept the "%5F" it held while writing its "_" as
		// "%5F" gave both one relation.
		one, other := pair[0], pair[1]
		for depth := 2; depth <= 3; depth++ {
			one, other = sessionflow.NodeIDIn("run", one, "a"), sessionflow.NodeIDIn("run", other, "a")
			if id := sessionflow.RelationID("cancels", "interrupt/1/1", one); id == sessionflow.RelationID("cancels", "interrupt/1/1", other) {
				t.Errorf("nested %d deep, the relations to %s and to %s are both %s", depth, one, other, id)
			}
			if id := sessionflow.NodeIDIn("run", one, "a"); id == sessionflow.NodeIDIn("run", other, "a") {
				t.Errorf("nested %d deep, the runs in %s and in %s are both %s", depth, one, other, id)
			}
		}
	}
}

// TestIDsAreWrittenAsThePageSays. Each id here is one the Session Flow page
// shows or describes. A part that holds no "%" and no "/" is written as it is,
// and a nested id that holds no "_" reads with each "/" as "_".
func TestIDsAreWrittenAsThePageSays(t *testing.T) {
	block := 2
	for _, c := range []struct{ got, want string }{
		{sessionflow.NodeID("tool", "toolu_01Ax"), "tool/toolu_01Ax"},
		{sessionflow.NodeID("talk", "main", "8d53"), "talk/main/8d53"},
		{sessionflow.NodeIDIn("run", "talk/main/8d53", "8d53"), "run/talk_main_8d53/8d53"},
		{sessionflow.LocatedID("segment", sessionflow.Ref{Seq: 2, Row: 7}, "at"), "segment/at/2/7"},
		{sessionflow.LocatedID("epoch", sessionflow.Ref{Seq: 3, Row: 9}, "main", "at"), "epoch/main/at/3/9"},
		{sessionflow.RefID("msg", sessionflow.Ref{Seq: 4, Row: 5, Block: &block}), "msg/4/5:2"},
		{sessionflow.RelationID("in_segment", "talk/main/8d53", "segment/at/2/7"), "rel/in_segment/talk_main_8d53/segment_at_2_7"},
		{sessionflow.UnresolvedID("tool_result", "toolu_01Bq"), "unres/tool_result/toolu_01Bq"},
		// A "%" and a "_" inside a nested id are escaped, and the nested id is
		// not escaped a second time.
		{sessionflow.RelationID("starts", "tool/toolu_01Ax", "stream/a308"), "rel/starts/tool_toolu%5F01Ax/stream_a308"},
		{sessionflow.RelationID("starts", sessionflow.NodeID("tool", "a/b%c"), "stream/c"), "rel/starts/tool_a%252Fb%2525c/stream_c"},
		{sessionflow.RelationID("cancels", "interrupt/1/1", "run/talk_main%5Fa/a"), "rel/cancels/interrupt_1_1/run_talk%5Fmain%255Fa_a"},
	} {
		if c.got != c.want {
			t.Errorf("%s, want %s", c.got, c.want)
		}
	}
}
