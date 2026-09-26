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

package view

import (
	"sort"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestTalkOrderIsATotalOrder.
//
// Talks are ordered by time, and by landed position where they have none.
// Deciding some pairs by time and others by position is not transitive:
// with positions A < B < C and times 30, none, 10 it says A before B, B
// before C, and C before A. A sort over that is undefined - it can leave
// two timed talks reversed and give a different order on the next read.
//
// So the order is total: every timed talk before every untimed one, timed
// talks by time, position only among equals.
func TestTalkOrderIsATotalOrder(t *testing.T) {
	talk := func(id string, seq uint64) *sessionflow.Node {
		return &sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/" + id},
			Kind: model.KindTalk, Ref: &sessionflow.Ref{Seq: seq, Row: 1}}
	}
	nodes := map[string]*sessionflow.Node{}
	for _, n := range []*sessionflow.Node{talk("a", 1), talk("b", 2), talk("c", 3)} {
		nodes[n.ID] = n
	}
	c := &Conversation{
		View: &sessionflow.View{Nodes: nodes},
		at:   map[[2]uint64]int64{{1, 1}: 30, {3, 1}: 10}, // b has no observed time
	}
	want := "talk/c talk/a talk/b"
	for i := 0; i < 50; i++ {
		var ids []string
		for _, n := range c.Talks() {
			ids = append(ids, n.ID)
		}
		if got := strings.Join(ids, " "); got != want {
			t.Fatalf("read %d: %s, want %s", i, got, want)
		}
	}
}

// TestRecordTimesSortByInstant. The plugin writes Go's RFC3339Nano, which
// drops trailing zeros, so the text of three times in order is not in order.
func TestRecordTimesSortByInstant(t *testing.T) {
	in := []string{"2026-09-26T10:00:00.11Z", "not a time", "2026-09-26T10:00:00.5Z", "2026-09-26T10:00:00.1Z", "2026-09-26T10:00:00Z"}
	sort.SliceStable(in, func(i, j int) bool { return compareTimes(in[i], in[j]) < 0 })
	want := "2026-09-26T10:00:00Z 2026-09-26T10:00:00.1Z 2026-09-26T10:00:00.11Z 2026-09-26T10:00:00.5Z not a time"
	if got := strings.Join(in, " "); got != want {
		t.Fatalf("%s, want %s", got, want)
	}
}
