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

package judge

import (
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// A talk's view digest moves with what a run is shown of its own work, and
// with nothing else: a later talk leaves it as it was; a field of one of its
// nodes, or of the sub-task it started, moves it; a relation that places a
// sub-task under it late moves it, and only it.
func TestATalksViewMovesWithItsOwnWork(t *testing.T) {
	base := TalkViews(threeTalks())
	if len(base) != 3 || base["talk/main/1"] == "" || base["talk/main/1"] == base["talk/main/2"] {
		t.Fatalf("views %v", base)
	}
	later := threeTalks()
	later.Talks = append(later.Talks, sessionview.Node{ID: "talk/main/4", Kind: "talk", Stream: "main", At: 2000, From: 2000, To: 2100,
		Children: []sessionview.Node{{ID: "input/4", Kind: model.KindMessageExternal, At: 2000, Text: "more"}}})
	for id, v := range base {
		if TalkViews(later)[id] != v {
			t.Fatalf("a later talk moved %s", id)
		}
	}
	child := threeTalks()
	child.Talks[1].Children[0].Text = "the sub-task said more"
	if v := TalkViews(child); v["talk/main/1"] == base["talk/main/1"] || v["talk/main/2"] != base["talk/main/2"] {
		t.Fatalf("a sub-task's node: %v", v)
	}
	// Before the relation that places sub-task a under talk 1 is known,
	// talk 1 shows less; talk 3 is the same either way.
	unplaced := threeTalks()
	unplaced.Relations = unplaced.Relations[1:]
	before := TalkViews(unplaced)
	if before["talk/main/1"] == base["talk/main/1"] || before["talk/main/3"] != base["talk/main/3"] {
		t.Fatalf("a starts relation placed late: %v", before)
	}
}

// A talk's work ends at the last of its records, the change and execution
// records joined to its steps included, which its span leaves out.
func TestATalksWorkEndsAtItsLastRecord(t *testing.T) {
	d := threeTalks()
	d.ToolExecutions = []sessionview.ToolExecution{{Step: "s3", Ref: sessionflow.Ref{Seq: 9, Row: 70}}}
	read := func(refs []sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		out := map[[2]uint64]*sessiondata.Record{}
		for _, r := range refs {
			out[[2]uint64{r.Seq, r.Row}] = &sessiondata.Record{Time: time.UnixMilli(5000).UTC().Format(time.RFC3339Nano)}
		}
		return out
	}
	l := Compute(d, "", nil, nil)
	ends := workEnds(d, l, read)
	if ends["talk/main/3"] != 5000 || ends["talk/main/1"] != 900 {
		t.Fatalf("ends %v", ends)
	}
}
