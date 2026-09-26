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
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
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

// TestAStreamsOriginsKeepOneOrder. A call the assembler could not tie to one
// stream leaves several candidates, and each is listed as an origin of the
// stream. The fold holds its relations in a map, so listing them in the order
// the map gave them changed the document from one read to the next: on a real
// conversation whose stream had three, five reads gave three orders. They are
// listed in the order the origin steps happened, by their position in the
// stream, and never by relation id, which says nothing about when: here the
// ids run the other way. Two steps at one position fall back to the id.
func TestAStreamsOriginsKeepOneOrder(t *testing.T) {
	node := func(id, kind, stream, attrs string, seq, row uint64) *sessionflow.Node {
		n := &sessionflow.Node{Entity: sessionflow.Entity{ID: id}, Kind: kind, Stream: stream,
			Ref: &sessionflow.Ref{Seq: seq, Row: row}}
		if attrs != "" {
			n.Attrs = json.RawMessage(attrs)
		}
		return n
	}
	nodes := map[string]*sessionflow.Node{}
	for _, n := range []*sessionflow.Node{
		node("stream/main", model.KindStream, "main", `{"role":"main"}`, 1, 1),
		node("stream/c1", model.KindStream, "c1", `{"role":"child"}`, 2, 1),
		node("tool/a", model.KindTool, "main", "", 1, 3),
		node("tool/b", model.KindTool, "main", "", 1, 1),
		node("tool/c", model.KindTool, "main", "", 1, 2),
		node("tool/d", model.KindTool, "main", "", 1, 2),
	} {
		nodes[n.ID] = n
	}
	rels := map[string]*sessionflow.Relation{}
	for _, r := range []struct{ id, from string }{{"rel/1", "tool/a"}, {"rel/2", "tool/b"}, {"rel/4", "tool/c"}, {"rel/3", "tool/d"}} {
		rels[r.id] = &sessionflow.Relation{Entity: sessionflow.Entity{ID: r.id}, Type: model.RelStarts, From: r.from, To: "stream/c1"}
	}
	c := &Conversation{View: &sessionflow.View{Nodes: nodes, Relations: rels}, Session: "s", zone: storage.NewZone(t.TempDir()),
		lanes: map[uint64]string{1: "stream/main", 2: "stream/c1"}}
	want := "tool/b tool/d tool/c tool/a"
	for i := 0; i < 50; i++ {
		var got []string
		for _, st := range streamRows(c, nil) {
			if st.ID != "stream/c1" {
				continue
			}
			for _, o := range st.OpenedBy {
				got = append(got, o.Step)
			}
		}
		if strings.Join(got, " ") != want {
			t.Fatalf("read %d: the origins are %v, want %s, by position", i, got, want)
		}
	}
}

// TestInOrderMergesLanesByTime. A position orders records inside one lane,
// and time orders lanes against each other: a child's file lands after its
// parent's, so by sequence all of its records would follow, though it ran in
// the middle. Inside a lane the position holds even where the times disagree,
// a timed item comes before an untimed one when lanes are merged, and an item
// with no position at all comes last.
func TestInOrderMergesLanesByTime(t *testing.T) {
	type item struct {
		name string
		p    point
	}
	at := func(lane string, seq, row uint64, t int64) point {
		return point{lane: lane, seq: seq, row: row, block: -1, at: t, ok: true}
	}
	items := []item{
		{"child-2", at("stream/c1", 5, 2, 25)},
		{"main-3", at("stream/main", 1, 3, 30)},
		{"main-1", at("stream/main", 1, 1, 10)},
		{"nowhere", point{}},
		{"child-1", at("stream/c1", 5, 1, 20)},
		{"main-2", at("stream/main", 1, 2, 5)}, // an earlier time than main-1, but after it in the stream
		{"run-1", at("run/r1", 9, 1, 0)},       // untimed: after every timed lane head
		{"tie-b", at("stream/b", 7, 1, 40)},    // two lane heads of one time: the earlier position first
		{"tie-a", at("stream/a", 6, 1, 40)},
	}
	want := "main-1 main-2 child-1 child-2 main-3 tie-a tie-b run-1 nowhere"
	for i := 0; i < 20; i++ {
		got := inOrder(items, func(x item) point { return x.p }, func(a, b item) bool { return a.name < b.name })
		var names []string
		for _, x := range got {
			names = append(names, x.name)
		}
		if strings.Join(names, " ") != want {
			t.Fatalf("got %v, want %s", names, want)
		}
		// the input order must not matter
		items[0], items[len(items)-1-i%len(items)] = items[len(items)-1-i%len(items)], items[0]
	}
}

// TestAStepsEdgesAreInTheOrderTheyHappened. A workflow launch starts several
// streams, and its run's journal names each child on a line of its own. The
// step lists the streams it started in the order of those lines, each
// relation at the earliest record its evidence names, never in the order of
// relation ids, which end in hashes of the agent ids.
func TestAStepsEdgesAreInTheOrderTheyHappened(t *testing.T) {
	launch := &sessionflow.Node{Entity: sessionflow.Entity{ID: "tool/launch"}, Kind: model.KindTool, Stream: "main",
		Ref: &sessionflow.Ref{Seq: 1, Row: 1}}
	rels := map[string]*sessionflow.Relation{}
	c := &Conversation{
		View:  &sessionflow.View{Nodes: map[string]*sessionflow.Node{launch.ID: launch}, Relations: rels},
		from:  map[string][]*sessionflow.Relation{},
		to:    map[string][]*sessionflow.Relation{},
		lanes: map[uint64]string{1: "stream/main", 2: "run/r1"},
	}
	// ids in one order, first-listed evidence in another, earliest evidence in a third
	for _, r := range []struct {
		id, to string
		rows   []uint64
	}{{"rel/a", "stream/c3", []uint64{7, 2}}, {"rel/b", "stream/c1", []uint64{3}}, {"rel/c", "stream/c2", []uint64{1}}} {
		rel := &sessionflow.Relation{Entity: sessionflow.Entity{ID: r.id}, Type: model.RelStarts, From: launch.ID, To: r.to}
		for _, row := range r.rows {
			rel.Evidence = append(rel.Evidence, sessionflow.Ref{Seq: 2, Row: row})
		}
		rels[r.id] = rel
		c.from[launch.ID] = append(c.from[launch.ID], rel)
	}
	var got []string
	for _, e := range c.step(launch, 0, nil).Edges {
		got = append(got, e.Other)
	}
	if want := "stream/c2 stream/c3 stream/c1"; strings.Join(got, " ") != want {
		t.Fatalf("the step's edges lead to %v, want %s", got, want)
	}

	// Evidence in two lanes: the relation happened at the earlier time, though
	// the other record has the smaller sequence. rel/x is supported at 361 in
	// the auxiliary stream and at 411 in the main one; rel/y at 380 in the main.
	c.from[launch.ID], c.View.Relations = nil, map[string]*sessionflow.Relation{}
	c.lanes[3] = "stream/aux"
	c.at = map[[2]uint64]int64{{2, 18}: 380, {2, 19}: 411, {3, 1}: 361}
	c.lanes[2] = "stream/main"
	for _, r := range []struct {
		id, to string
		ev     []sessionflow.Ref
	}{{"rel/x", "stream/x", []sessionflow.Ref{{Seq: 2, Row: 19}, {Seq: 3, Row: 1}}}, {"rel/y", "stream/y", []sessionflow.Ref{{Seq: 2, Row: 18}}}} {
		rel := &sessionflow.Relation{Entity: sessionflow.Entity{ID: r.id}, Type: model.RelStarts, From: launch.ID, To: r.to, Evidence: r.ev}
		c.View.Relations[r.id] = rel
		c.from[launch.ID] = append(c.from[launch.ID], rel)
	}
	got = nil
	for _, e := range c.step(launch, 0, nil).Edges {
		got = append(got, e.Other)
	}
	if want := "stream/x stream/y"; strings.Join(got, " ") != want {
		t.Fatalf("the step's edges lead to %v, want %s: a relation is at its earliest record by time across lanes", got, want)
	}
}

// TestEachStreamsOriginsAreOrderedOnTheirOwn. The origins of every stream
// were once merged in one pass and then split by stream, and a merge over a
// subset is not the subset's own: an untimed step holds back the rest of its
// lane, so a step that started another stream moved this one's origins.
// Here A1, untimed, starts X; A2 at time 10 and B1 at time 50 start S. A0,
// untimed and in no stream, also starts S: it is not listed, and it must not
// move the origins that are.
func TestEachStreamsOriginsAreOrderedOnTheirOwn(t *testing.T) {
	step := func(id, stream string, seq, row uint64) *sessionflow.Node {
		return &sessionflow.Node{Entity: sessionflow.Entity{ID: id}, Kind: model.KindTool, Stream: stream,
			Ref: &sessionflow.Ref{Seq: seq, Row: row}}
	}
	nodes := map[string]*sessionflow.Node{}
	for _, n := range []*sessionflow.Node{
		{Entity: sessionflow.Entity{ID: "stream/A"}, Kind: model.KindStream, Stream: "A", Attrs: json.RawMessage(`{"role":"main"}`)},
		{Entity: sessionflow.Entity{ID: "stream/B"}, Kind: model.KindStream, Stream: "B"},
		{Entity: sessionflow.Entity{ID: "stream/S"}, Kind: model.KindStream, Stream: "S"},
		{Entity: sessionflow.Entity{ID: "stream/X"}, Kind: model.KindStream, Stream: "X"},
		step("tool/a0", "", 1, 1), step("tool/a1", "A", 1, 1), step("tool/a2", "A", 1, 2), step("tool/b1", "B", 2, 1),
	} {
		nodes[n.ID] = n
	}
	rels := map[string]*sessionflow.Relation{}
	for _, r := range []struct{ id, from, to string }{
		{"rel/0", "tool/a0", "stream/S"}, {"rel/1", "tool/a1", "stream/X"}, {"rel/2", "tool/a2", "stream/S"}, {"rel/3", "tool/b1", "stream/S"},
	} {
		rels[r.id] = &sessionflow.Relation{Entity: sessionflow.Entity{ID: r.id}, Type: model.RelStarts, From: r.from, To: r.to}
	}
	c := &Conversation{View: &sessionflow.View{Nodes: nodes, Relations: rels}, Session: "s", zone: storage.NewZone(t.TempDir()),
		at: map[[2]uint64]int64{{1, 2}: 10, {2, 1}: 50}, lanes: map[uint64]string{1: "stream/A", 2: "stream/B"}}
	found := false
	for _, st := range streamRows(c, nil) {
		if st.ID != "stream/S" {
			continue
		}
		found = true
		var got []string
		for _, o := range st.OpenedBy {
			got = append(got, o.Step)
		}
		if want := "tool/a2 tool/b1"; strings.Join(got, " ") != want {
			t.Fatalf("S's origins are %v, want %s", got, want)
		}
	}
	if !found {
		t.Fatal("stream S is not listed")
	}
}

// TestRecordsOfOneTimeAreInTheOrderTheyWereRead. Two change records, or two
// execution records, can name the same time. Then the one read first is
// listed first, never the one whose id sorts first; and of two change records
// the runtime's still comes before the plugin's.
func TestRecordsOfOneTimeAreInTheOrderTheyWereRead(t *testing.T) {
	c := &Conversation{lanes: map[uint64]string{4: "stream/main"}}
	at := func(row uint64) sessionflow.Ref { return sessionflow.Ref{Seq: 4, Row: row} }
	const when = "2026-01-01T00:00:01Z"
	// the same instant spelled another way is the same time
	const alsoWhen = "2026-01-01T00:00:01.0Z"
	change := func(id, by string, row uint64, time string) sessionview.WorkspaceChange {
		return sessionview.WorkspaceChange{Ref: at(row), Record: changes.Record{ID: id, CapturedBy: by, Time: time}}
	}
	cs := []sessionview.WorkspaceChange{
		change("a", "asz-plugin", 3, alsoWhen), change("b", "asz-plugin", 1, when), change("z", changes.CapturedByClaudeCode, 9, alsoWhen),
	}
	sort.SliceStable(cs, func(i, j int) bool { return c.changeBefore(&cs[i], &cs[j]) })
	if got := cs[0].ID + cs[1].ID + cs[2].ID; got != "zba" {
		t.Errorf("the change records are in the order %s, want zba: the runtime's, then by where each was read", got)
	}
	observed := func(id string, row uint64, time string) sessionview.ToolExecution {
		return sessionview.ToolExecution{Ref: at(row), Record: execution.Record{ID: id, Time: time}}
	}
	es := []sessionview.ToolExecution{observed("a", 3, alsoWhen), observed("b", 1, when)}
	sort.SliceStable(es, func(i, j int) bool { return c.executionBefore(&es[i], &es[j]) })
	if got := es[0].ID + es[1].ID; got != "ba" {
		t.Errorf("the execution records are in the order %s, want ba, by where each was read", got)
	}
	// times that do not parse come after one that does, and between themselves
	// by where each was read, not by their text
	es = []sessionview.ToolExecution{observed("c", 2, "a time"), observed("d", 1, "b time"), observed("e", 5, when)}
	sort.SliceStable(es, func(i, j int) bool { return c.executionBefore(&es[i], &es[j]) })
	if got := es[0].ID + es[1].ID + es[2].ID; got != "edc" {
		t.Errorf("the execution records are in the order %s, want edc", got)
	}
}
