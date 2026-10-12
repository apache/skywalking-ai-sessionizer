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

package kiro_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/kiro"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// The fixtures are measured sessions of kiro-cli 2.28.0 on the V3 engine
// (@kiro/agent 0.66.26), and of Kiro Crew 0.7.2 on it. The kit's sessions keep
// their text, which the kit wrote, with paths, the engine's system prompt and
// reasoning signatures replaced. The sessions from real use keep only their
// record structure: every text a person, the agent or a tool wrote is
// replaced, and their headers keep only the fields every header has.
const fixtures = "testdata"

const (
	// Two subagents started in parallel in one turn.
	subagents = "sess_acd7b622-5c65-468e-a288-6bb404e112f2"
	// Hooks on every step, a failed tool, a subagent whose file has hook
	// records without the child's id, then a compaction and a turn after it.
	combined = "sess_d3a5510d-40b8-4a77-be1c-bf31a17f584d"
	// A turn the person cut with Ctrl-C while a tool ran.
	cancelled = "sess_03390a84-1c44-4785-bcd2-d4b15891b24a"
	// A tool the person denied when asked to approve it.
	denied = "sess_77537f32-41e4-4bfc-b412-e80db5ec1d51"
	// Kiro Crew: two turns an expired login failed, then messages sent
	// during two later turns, and the engine's text around them.
	crew = "sess_5f20ccf4-b254-4bdb-945b-080e051eb7f5"
	// A Kiro Crew background prompt with two images.
	images = "sess_8272aa81-fa49-4d34-aa24-8eff99377673"
)

var allFixtures = []string{subagents, combined, cancelled, denied, crew, images}

// root is a storage root the fixtures, or a copy of some, are landed into.
type root struct {
	t    *testing.T
	zone *storage.Zone
	col  *kiro.Collector
}

func newRoot(t *testing.T, src string) *root {
	t.Helper()
	z := storage.NewZone(t.TempDir())
	return &root{t: t, zone: z, col: kiro.New(src, z, 0)}
}

func (r *root) collect() *kiro.Stats {
	r.t.Helper()
	st, err := r.col.CollectAll(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if !st.Complete() {
		r.t.Fatalf("pass not complete: %+v", st)
	}
	return st
}

func (r *root) parse(id string) *parse.Round {
	r.t.Helper()
	res, err := parse.Session(r.zone, parse.Options{Conversation: id, Session: id})
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *root) doc(id string) *sessionview.Conversation {
	r.t.Helper()
	c, err := view.New(r.zone, nil).Load(id)
	if err != nil {
		r.t.Fatal(err)
	}
	d, err := c.Build()
	if err != nil {
		r.t.Fatal(err)
	}
	if d.Summary.State != sessionview.StateVerified || len(d.Summary.Problems) > 0 {
		r.t.Fatalf("%s: %s %v", id, d.Summary.State, d.Summary.Problems)
	}
	return d
}

// landed is every fixture collected and parsed once.
func landed(t *testing.T) *root {
	t.Helper()
	r := newRoot(t, fixtures)
	r.collect()
	for _, id := range allFixtures {
		r.parse(id)
	}
	return r
}

// nodes is every node of a document, flattened.
func nodes(d *sessionview.Conversation) []sessionview.Node {
	var out []sessionview.Node
	var walk func(ns []sessionview.Node)
	walk = func(ns []sessionview.Node) {
		for _, n := range ns {
			out = append(out, n)
			walk(n.Children)
		}
	}
	walk(d.Talks)
	walk(d.Loose)
	return out
}

func ofKind(d *sessionview.Conversation, kind string) []sessionview.Node {
	var out []sessionview.Node
	for _, n := range nodes(d) {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// TestGlossaryCoversTheWholeVocabulary: every name the model can emit has an
// entry, including the ones saying Kiro has no word for it.
func TestGlossaryCoversTheWholeVocabulary(t *testing.T) {
	g := kiro.Glossary()
	var missing []string
	for _, name := range model.Vocabulary() {
		if _, ok := g.Lookup(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d name(s) have no term:\n  %v", len(missing), missing)
	}
}

// Discovery collects V3 sessions of the one measured schema, and counts what
// it leaves: kiro-cli's V2 sessions, and V3 sessions of another schema or with
// a header that is not JSON. A session with a header and no record is not a
// conversation yet, and is neither collected nor counted.
func TestDiscoverReadsOnlyTheMeasuredSchema(t *testing.T) {
	sessions, skipped, warnings, err := kiro.Discover(fixtures)
	if err != nil || len(warnings) > 0 {
		t.Fatal(err, warnings)
	}
	if len(sessions) != len(allFixtures) || skipped.V2 != 1 || skipped.Total() != 1 {
		t.Fatalf("fixtures: %d sessions, skipped %+v", len(sessions), skipped)
	}

	src := t.TempDir()
	write := func(id, header string, records bool) {
		dir := filepath.Join(src, "sessions", "0123456789abcdef", id)
		mkfile(t, filepath.Join(dir, "session.json"), header)
		if records {
			mkfile(t, filepath.Join(dir, "messages.jsonl"), `{"id":"a","timestamp":"2026-10-08T00:00:00.000Z","payload":{"type":"agent_note","content":"x"}}`+"\n")
		}
	}
	// A transcript that is there and still empty holds no record either.
	mkfile(t, filepath.Join(src, "sessions", "0123456789abcdef", "sess_00000000-0000-4000-8000-000000000006", "session.json"),
		`{"schemaVersion":"1.0.0"}`)
	mkfile(t, filepath.Join(src, "sessions", "0123456789abcdef", "sess_00000000-0000-4000-8000-000000000006", "messages.jsonl"), "")
	write("sess_00000000-0000-4000-8000-000000000001", `{"schemaVersion":"1.0.0","title":"kept"}`, true)
	write("sess_00000000-0000-4000-8000-000000000002", `{"schemaVersion":"2.0.0"}`, true)
	write("sess_00000000-0000-4000-8000-000000000003", `{"title":"no schema"}`, true)
	write("sess_00000000-0000-4000-8000-000000000004", `{"schemaVersion":`, true)
	write("sess_00000000-0000-4000-8000-000000000005", `{"schemaVersion":"1.0.0"}`, false)
	sessions, skipped, _, err = kiro.Discover(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "sess_00000000-0000-4000-8000-000000000001" {
		t.Fatalf("collected %+v", sessions)
	}
	want := kiro.Skipped{Schemas: map[string]int{"2.0.0": 1, "none": 1}, Unreadable: 1}
	if !reflect.DeepEqual(skipped, want) {
		t.Fatalf("skipped %+v, want %+v", skipped, want)
	}
}

// The node kinds of each fixture, each counted by hand from its lines. In the
// Crew chat, for one: four prompts and three messages sent during turns are
// the external messages, and the engine's note and its two texts around those
// messages are the injections.
var wantKinds = map[string]map[string]int{
	subagents: {"agent.call": 2, "agent.output": 2, "context.injection": 2, "epoch": 3, "message.assistant": 5,
		"message.external": 1, "run": 3, "segment": 1, "session": 1, "stream": 3, "talk": 3, "thinking": 6, "tool": 4,
		"turn.duration": 1},
	combined: {"agent.call": 1, "agent.output": 1, "context.injection": 3, "control.notice": 16, "epoch": 3,
		"epoch.boundary": 1, "epoch.summary": 1, "message.assistant": 6, "message.external": 3, "run": 4, "segment": 1,
		"session": 1, "stream": 2, "talk": 4, "thinking": 8, "tool": 5, "turn.duration": 3},
	cancelled: {"context.injection": 2, "control.notice": 1, "epoch": 1, "message.external": 1, "run": 1, "segment": 1,
		"session": 1, "stream": 1, "talk": 1, "thinking": 1, "tool": 1, "turn.duration": 1},
	denied: {"context.injection": 2, "epoch": 1, "message.assistant": 1, "message.external": 1, "run": 1, "segment": 1,
		"session": 1, "stream": 1, "talk": 1, "thinking": 2, "tool": 1, "turn.duration": 1},
	crew: {"context.injection": 3, "control.notice": 2, "epoch": 1, "error.api": 2, "message.assistant": 5,
		"message.external": 7, "run": 4, "segment": 2, "session": 1, "stream": 1, "talk": 4, "thinking": 6, "tool": 6,
		"turn.duration": 4},
	images: {"context.injection": 2, "epoch": 1, "message.assistant": 1, "message.external": 1, "run": 1, "segment": 1,
		"session": 1, "stream": 1, "talk": 1, "thinking": 1, "turn.duration": 1},
}

// Every fixture lands, parses to a verified conversation with the steps its
// records hold, and lands and parses again with nothing new: no record lands
// twice and no round is written.
func TestFixturesLandAndParseTwice(t *testing.T) {
	r := newRoot(t, fixtures)
	st := r.collect()
	if st.Sessions != len(allFixtures) || st.Held != 0 || len(st.Changed) != len(allFixtures) {
		t.Fatalf("first pass: %+v", st)
	}
	first := map[string][]byte{}
	for _, id := range allFixtures {
		r.parse(id)
		d := r.doc(id)
		if d.Summary.Title == "" {
			t.Errorf("%s: no title", id)
		}
		if !reflect.DeepEqual(d.Summary.Kinds, wantKinds[id]) {
			t.Errorf("%s: kinds\n got %v\nwant %v", id, d.Summary.Kinds, wantKinds[id])
		}
		first[id], _ = json.Marshal(d)
		rep, err := verify.Session(r.zone, id)
		if err != nil || rep.Problems != 0 {
			t.Fatalf("%s: verify: %v %+v", id, err, rep)
		}
	}
	if st := r.collect(); st.Records != 0 || len(st.Changed) != 0 {
		t.Fatalf("second pass landed again: %+v", st)
	}
	for _, id := range allFixtures {
		if res := r.parse(id); res.Changed() {
			t.Errorf("%s: a second parse with nothing new wrote a round", id)
		}
		again, _ := json.Marshal(r.doc(id))
		if !bytes.Equal(first[id], again) {
			t.Errorf("%s: the conversation changed with nothing new", id)
		}
	}
}

// A pass that dies between landing and committing its cursors lands the same
// records again. Assembly removes the copies by record id, so the conversation
// is the one a clean pass gives: a copy has the id of the record it repeats,
// because a record's id is made from its own bytes.
func TestRelandedRecordsFoldToTheSameConversation(t *testing.T) {
	clean := landed(t)
	r := newRoot(t, fixtures)
	r.collect()
	err := filepath.Walk(r.zone.Root(), func(p string, _ os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".cursor") {
			return os.Remove(p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if st := r.collect(); st.Records == 0 {
		t.Fatal("nothing landed again after the cursors were lost")
	}
	for _, id := range allFixtures {
		r.parse(id)
		got, want := r.doc(id).Summary, clean.doc(id).Summary
		if !reflect.DeepEqual(got.Kinds, want.Kinds) || !reflect.DeepEqual(got.RelationTypes, want.RelationTypes) {
			t.Errorf("%s: relanded %v %v, clean %v %v", id, got.Kinds, got.RelationTypes, want.Kinds, want.RelationTypes)
		}
	}
}

// Every tool call joins its result exactly, and a call that failed or was
// denied says so.
func TestToolsJoinTheirResults(t *testing.T) {
	r := landed(t)
	for _, id := range allFixtures {
		for _, n := range append(ofKind(r.doc(id), model.KindTool), ofKind(r.doc(id), model.KindAgentCall)...) {
			if n.RequestToResultBy != model.ExactUnique || n.ResultState != model.ContentAvailable {
				t.Errorf("%s: %s joined %q, result %q", id, n.ID, n.RequestToResultBy, n.ResultState)
			}
		}
	}
	// The denied call and the failed one report it.
	for id, tool := range map[string]string{
		denied:   "tool/run_command_toolu_bdrk_01VEBiG5BGUW2Yd6gFizBRkt",
		combined: "tool/run_command_toolu_bdrk_01NbKeQ4aK6ZqmiK6F8AJ2TN",
	} {
		found := false
		for _, n := range ofKind(r.doc(id), model.KindTool) {
			if strings.HasPrefix(n.ID, tool) {
				found = true
				if n.Failed == nil || !*n.Failed {
					t.Errorf("%s: %s does not say it failed", id, n.ID)
				}
			}
		}
		if !found {
			t.Errorf("%s: no %s", id, tool)
		}
	}
}

// A subagent is joined by the id the parent's start and end records and the
// child's own file share: the start is the call, the end is its result, and
// the child's stream is its own talk with its output.
func TestSubagentsAreTheirCallsChildren(t *testing.T) {
	r := landed(t)
	d := r.doc(subagents)
	calls := ofKind(d, model.KindAgentCall)
	if len(calls) != 2 {
		t.Fatalf("agent calls: %d", len(calls))
	}
	starts := map[string]string{}
	for _, rel := range d.Relations {
		if rel.Type == model.RelStarts {
			starts[rel.From] = rel.To
		}
	}
	for _, c := range calls {
		child := strings.TrimPrefix(c.ID, "tool/")
		if starts[c.ID] != "stream/"+child {
			t.Errorf("%s starts %q", c.ID, starts[c.ID])
		}
	}
	if n := len(ofKind(d, model.KindAgentOutput)); n != 2 {
		t.Errorf("agent outputs: %d", n)
	}
	for _, s := range d.Streams {
		if s.Role != "main" && s.Parent == "" {
			t.Errorf("child %s has no parent", s.Name)
		}
	}
	// A child's hook records carry no child id, and still belong to its
	// stream, because the file names it.
	c := r.doc(combined)
	child := "5d13f3d7-73ae-431d-847f-04729b40503e"
	notices := 0
	for _, n := range ofKind(c, model.KindControlNotice) {
		if n.Stream == child {
			notices++
		}
	}
	if notices != 4 {
		t.Errorf("hook notices in the child's stream: %d, want 4", notices)
	}
}

// A compaction is a reset: the tombstone opens a new epoch, the summary sits
// in it, and the turn after it is a talk of the new epoch.
func TestCompactionIsAnEpoch(t *testing.T) {
	d := landed(t).doc(combined)
	if len(ofKind(d, model.KindEpochBoundary)) != 1 || len(ofKind(d, model.KindEpochSummary)) != 1 {
		t.Fatalf("kinds %v", d.Summary.Kinds)
	}
	if d.Summary.RelationTypes[model.RelFollows] != 1 {
		t.Fatalf("relations %v", d.Summary.RelationTypes)
	}
	last := d.Talks[len(d.Talks)-1]
	for _, t0 := range d.Talks {
		if t0.Stream == "main" {
			last = t0
		}
	}
	if !strings.HasPrefix(last.Parent, "epoch/main/tombstone_summarize_") {
		t.Errorf("the turn after the compaction is in %s", last.Parent)
	}
}

// A message sent while a turn runs belongs to that turn, though it carries no
// turn id and its line comes before the turn's next record. So does the
// engine's text that carries it to the model. The note that the messages were
// consumed is kept and is no step.
func TestSteeringStaysInItsTurn(t *testing.T) {
	d := landed(t).doc(crew)
	if len(d.Talks) != 4 {
		t.Fatalf("talks: %d", len(d.Talks))
	}
	for _, turn := range []string{"f23b2b56-2d8e-4cb3-bb0c-fe9eefd1176e", "46b63358-201f-4475-b887-927b5281b962"} {
		var run *sessionview.Node
		for _, n := range nodes(d) {
			if n.Kind == model.KindRun && strings.HasSuffix(n.ID, "/"+turn) {
				n := n
				run = &n
			}
		}
		if run == nil {
			t.Fatalf("no run %s", turn)
		}
		inputs, injected := 0, 0
		for _, c := range run.Children {
			switch c.Kind {
			case model.KindMessageExternal:
				inputs++
			case model.KindContextInjection:
				injected++
			}
		}
		// The prompt, then one or two steering messages; one text around them.
		if inputs < 2 || injected != 1 {
			t.Errorf("run %s: %d inputs, %d injected", turn, inputs, injected)
		}
	}
	if n := len(ofKind(d, model.KindErrorAPI)); n != 2 {
		t.Errorf("errors: %d", n)
	}
	// Each failed turn says it ended so.
	if n := len(ofKind(d, model.KindControlNotice)); n != 2 {
		t.Errorf("notices: %d", n)
	}
}

// A turn the person cancelled is a notice, and the turn's own measure of its
// length is read.
func TestCancelledTurn(t *testing.T) {
	d := landed(t).doc(cancelled)
	if n := len(ofKind(d, model.KindControlNotice)); n != 1 {
		t.Errorf("notices: %d", n)
	}
	durations := ofKind(d, model.KindTurnDuration)
	if len(durations) != 1 || durations[0].DurationMS != 12026 {
		t.Errorf("durations: %+v", durations)
	}
}

// A prompt held for its turn is counted once, by the call that lands the
// last line before it, though a small budget cuts the lines before it into
// several files and each cut drains the source again.
func TestAHeldPromptIsCountedOnceAcrossCuts(t *testing.T) {
	lines := fixtureLines(t, combined)
	src := t.TempDir()
	dir := filepath.Join(src, "sessions", "01fceef077f333f9", combined)
	header, err := os.ReadFile(filepath.Join(fixtures, "sessions", "01fceef077f333f9", combined, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	mkfile(t, filepath.Join(dir, "session.json"), string(header))
	// The first turn whole, then the next prompt, whose turn is not written yet.
	mkfile(t, filepath.Join(dir, "messages.jsonl"), strings.Join(lines[:27], "\n")+"\n")

	z := storage.NewZone(t.TempDir())
	col := kiro.New(src, z, 2048)
	held, records := 0, 0
	for i := 0; ; i++ {
		st, err := col.CollectAll(nil)
		if err != nil {
			t.Fatal(err)
		}
		held, records = held+st.Held, records+st.Records
		if st.Complete() {
			break
		}
		if i > 10 {
			t.Fatal("collection does not finish")
		}
	}
	files, err := storage.LandedFiles(z, combined)
	if err != nil {
		t.Fatal(err)
	}
	if held != 1 || records != 27 || len(files) < 3 {
		t.Fatalf("held %d, landed %d records in %d files; want the prompt held once and the rest landed in several files",
			held, records, len(files))
	}
}

// A prompt waits for the turn it opens. Landed before its turn's first record
// is written, it would sit outside that turn for good, since a landed record
// is never written again.
func TestPromptWaitsForItsTurn(t *testing.T) {
	lines := fixtureLines(t, cancelled)
	src := t.TempDir()
	dir := filepath.Join(src, "sessions", "98613594c3327868", cancelled)
	header, err := os.ReadFile(filepath.Join(fixtures, "sessions", "98613594c3327868", cancelled, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	mkfile(t, filepath.Join(dir, "session.json"), string(header))
	messages := filepath.Join(dir, "messages.jsonl")
	// The engine's note and the prompt, and nothing of the turn yet.
	mkfile(t, messages, strings.Join(lines[:2], "\n")+"\n")

	r := newRoot(t, src)
	st := r.collect()
	if st.Held != 1 || st.Records != 1 {
		t.Fatalf("first pass: %+v, want the header landed and the prompt held", st)
	}
	mkfile(t, messages, strings.Join(lines, "\n")+"\n")
	if st := r.collect(); st.Held != 0 || st.Records != len(lines) {
		t.Fatalf("second pass: %+v", st)
	}
	r.parse(cancelled)
	d := r.doc(cancelled)
	if len(d.Talks) != 1 || d.Talks[0].Runs != 1 {
		t.Fatalf("talks %+v", d.Talks)
	}
	run := d.Talks[0].Children[0]
	kinds := map[string]bool{}
	for _, c := range run.Children {
		kinds[c.Kind] = true
	}
	if run.Kind != model.KindRun || !kinds[model.KindMessageExternal] || !kinds[model.KindContextInjection] {
		t.Fatalf("the prompt and the note are not in the turn's run: %+v", run)
	}
}

// copyCancelled writes the cancelled fixture's header and the given transcript
// into a new source root, and returns the root and the transcript's path.
func copyCancelled(t *testing.T, transcript string) (string, string) {
	t.Helper()
	src := t.TempDir()
	dir := filepath.Join(src, "sessions", "98613594c3327868", cancelled)
	header, err := os.ReadFile(filepath.Join(fixtures, "sessions", "98613594c3327868", cancelled, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	mkfile(t, filepath.Join(dir, "session.json"), string(header))
	messages := filepath.Join(dir, "messages.jsonl")
	mkfile(t, messages, transcript)
	return src, messages
}

// promptIsInItsTurn fails unless the session's one prompt is inside its run.
func promptIsInItsTurn(t *testing.T, r *root) {
	t.Helper()
	r.parse(cancelled)
	d := r.doc(cancelled)
	if len(d.Talks) != 1 || d.Talks[0].Runs != 1 {
		t.Fatalf("talks %+v", d.Talks)
	}
	run := d.Talks[0].Children[0]
	for _, c := range run.Children {
		if c.Kind == model.KindMessageExternal {
			return
		}
	}
	t.Fatalf("the prompt is not in the turn's run: %+v", run)
}

// A pass reads a file in windows cut by size, so the turn a prompt opens can lie
// past the window the prompt is in. The lines after the window are read for it,
// so the prompt lands in its turn even when the file has been still long enough
// that a prompt with no turn in the file would land without one.
func TestATurnPastTheWindowIsFound(t *testing.T) {
	lines := fixtureLines(t, cancelled)
	src, messages := copyCancelled(t, strings.Join(lines, "\n")+"\n")
	still := time.Now().Add(-6 * time.Minute)
	if err := os.Chtimes(messages, still, still); err != nil {
		t.Fatal(err)
	}
	r := newRoot(t, src)
	// The first window ends right after the prompt; its turn starts on the next line.
	r.col.MaxDelta = int64(len(lines[0]) + len(lines[1]) + 2)
	r.collect()
	promptIsInItsTurn(t, r)
}

// A turn line with no newline yet may still be being written, so it is not read:
// the prompt waits for it, and lands in its turn once the line is finished.
func TestAPromptWaitsForATurnLineStillBeingWritten(t *testing.T) {
	lines := fixtureLines(t, cancelled)
	src, messages := copyCancelled(t, strings.Join(lines[:3], "\n"))
	r := newRoot(t, src)
	if st := r.collect(); st.Held != 1 || st.Records != 1 {
		t.Fatalf("first pass: %+v, want the header landed and the prompt held", st)
	}
	mkfile(t, messages, strings.Join(lines, "\n")+"\n")
	r.collect()
	promptIsInItsTurn(t, r)
}

// Kiro keeps a session's title in its header and rewrites the header when the
// title changes. Each version lands as a snapshot in the main stream, and the
// conversation takes the latest one's title.
func TestTheLatestHeaderNamesTheConversation(t *testing.T) {
	lines := fixtureLines(t, cancelled)
	src, messages := copyCancelled(t, strings.Join(lines, "\n")+"\n")
	header := filepath.Join(filepath.Dir(messages), "session.json")
	r := newRoot(t, src)
	r.collect()
	r.parse(cancelled)
	if got := r.doc(cancelled).Summary.Title; got != "Execute Sleep and Echo Command" {
		t.Fatalf("title %q, want the header's", got)
	}
	mkfile(t, header, `{"schemaVersion":"1.0.0","title":"Renamed by the person"}`)
	r.collect()
	r.parse(cancelled)
	if got := r.doc(cancelled).Summary.Title; got != "Renamed by the person" {
		t.Fatalf("title %q, want the latest header's", got)
	}
}

// A prompt whose turn never starts, because the engine stopped before it, lands
// with no turn once the file has been still for a while.
func TestAbandonedPromptLandsWithoutATurn(t *testing.T) {
	lines := fixtureLines(t, cancelled)
	src := t.TempDir()
	dir := filepath.Join(src, "sessions", "98613594c3327868", cancelled)
	mkfile(t, filepath.Join(dir, "session.json"), `{"schemaVersion":"1.0.0","title":"abandoned"}`)
	messages := filepath.Join(dir, "messages.jsonl")
	mkfile(t, messages, strings.Join(lines[:2], "\n")+"\n")

	r := newRoot(t, src)
	still := time.Now().Add(-6 * time.Minute)
	if err := os.Chtimes(messages, still, still); err != nil {
		t.Fatal(err)
	}
	if st := r.collect(); st.Held != 0 || st.Records != 3 {
		t.Fatalf("pass: %+v, want the header, the note and the prompt landed", st)
	}
	r.parse(cancelled)
	if n := len(ofKind(r.doc(cancelled), model.KindMessageExternal)); n != 1 {
		t.Fatalf("prompts: %d", n)
	}
}

// A file that fails to land leaves no entry in the index and no gap in the
// landed numbers. The other source lands in the same pass, so the session's
// index and state are saved: had the failed file's records entered the index
// during the write, it would name records that never landed, and had the file
// kept its number, every later round would report that number missing. The
// transcript is a window of a growing file and the header a snapshot, so each
// is made to fail in turn.
func TestAFileThatFailsToLandLeavesNoIndexEntry(t *testing.T) {
	for _, prefix := range []string{"transcript", "meta"} {
		t.Run(prefix, func(t *testing.T) { failToLand(t, prefix) })
	}
}

func failToLand(t *testing.T, prefix string) {
	src := t.TempDir()
	dir := filepath.Join(src, "sessions", "98613594c3327868", cancelled)
	header, err := os.ReadFile(filepath.Join(fixtures, "sessions", "98613594c3327868", cancelled, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	mkfile(t, filepath.Join(dir, "session.json"), string(header))
	mkfile(t, filepath.Join(dir, "messages.jsonl"), strings.Join(fixtureLines(t, cancelled), "\n")+"\n")

	r := newRoot(t, src)
	at := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	r.col.Now = func() time.Time { return at }
	// A directory with something in it where the file would go makes its
	// rename fail. The sequence is recovered from landed files only, so the
	// blockers do not move it.
	stream := r.zone.StreamDir(cancelled, storage.StreamMain)
	var blockers []string
	for seq := uint64(1); seq <= 3; seq++ {
		b := filepath.Join(stream, storage.LandedName(prefix, storage.Stamp(at), seq))
		mkfile(t, filepath.Join(b, "keep"), "")
		blockers = append(blockers, b)
	}
	st, err := r.col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 || st.SourcesLanded != 1 {
		t.Fatalf("pass: %+v, want one source landed and the %s failed", st, prefix)
	}
	indexHoldsOnlyLandedRecords(t, r.zone, cancelled)

	for _, b := range blockers {
		if err := os.RemoveAll(b); err != nil {
			t.Fatal(err)
		}
	}
	r.col.Now = time.Now
	r.collect()
	indexHoldsOnlyLandedRecords(t, r.zone, cancelled)
	r.parse(cancelled)
	r.doc(cancelled)
}

// indexHoldsOnlyLandedRecords fails unless the session's saved index has one
// entry for each landed record, and each in the file it names. A count is what
// catches an entry left by a failed file whose number the next file took: that
// entry names a file that exists.
func indexHoldsOnlyLandedRecords(t *testing.T, z *storage.Zone, id string) {
	t.Helper()
	ix, ok, err := index.Load(z.IndexDir(id), id)
	if err != nil || !ok {
		t.Fatalf("index: ok=%v err=%v", ok, err)
	}
	records := map[uint32]int{}
	err = filepath.WalkDir(z.SessionDir(id), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".sd") {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		rd, err := sessiondata.NewReader(f)
		if err != nil {
			return err
		}
		seq := uint32(rd.Header().Seq)
		for {
			if _, err := rd.Next(); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
			records[seq]++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	inIndex := map[uint32]int{}
	for _, e := range ix.Entries {
		inIndex[e.Seq]++
	}
	if !reflect.DeepEqual(inIndex, records) {
		t.Fatalf("index entries per file %v, landed records per file %v", inIndex, records)
	}
}

// Record ids are unique in a session and the same for the same bytes, though
// the engine reuses its own ids across records.
func TestRecordIDsAreUniqueAndStable(t *testing.T) {
	for _, id := range allFixtures {
		seen := map[string]string{}
		shared := 0
		natives := map[string]int{}
		for i, ln := range fixtureLines(t, id) {
			src := kiro.Source{Kind: kiro.SrcMessages, Session: id, Stream: "main"}
			a := kiro.Convert(src, uint64(i+1), 0, []byte(ln), "")
			b := kiro.Convert(src, uint64(i+1), 0, []byte(ln), "")
			if a.ID != b.ID {
				t.Fatalf("%s line %d: id %q then %q", id, i+1, a.ID, b.ID)
			}
			if prev, dup := seen[a.ID]; dup && prev != ln {
				t.Fatalf("%s line %d: id %q names two records", id, i+1, a.ID)
			}
			seen[a.ID] = ln
			native, _, _ := strings.Cut(a.ID, "#")
			natives[native]++
			if natives[native] == 2 {
				shared++
			}
		}
		if id == crew && shared == 0 {
			t.Errorf("%s: no native id is shared, so this test proves nothing", id)
		}
	}
}

// The engine writes "..." for reasoning it did not keep, and reads that as no
// reasoning itself. So it is no text here. The signature is named as dropped.
func TestReasoningPlaceholderIsNoText(t *testing.T) {
	var kept, withheld int
	for _, ln := range fixtureLines(t, subagents) {
		rec := kiro.Convert(kiro.Source{Kind: kiro.SrcMessages}, 1, 0, []byte(ln), "")
		for _, p := range rec.Parts {
			if p.Kind != sessiondata.PartReasoning {
				continue
			}
			if p.Text == "..." {
				t.Fatalf("the placeholder became text: %+v", p)
			}
			if p.State == model.ContentUnavailable {
				withheld++
				if len(rec.Dropped) != 1 || rec.Dropped[0].What != "reasoning signature" {
					t.Fatalf("dropped %+v", rec.Dropped)
				}
			} else {
				kept++
			}
		}
	}
	if withheld == 0 || kept != 0 {
		t.Fatalf("withheld %d, kept %d", withheld, kept)
	}
}

// A prompt's images are media parts, their bytes as they came.
func TestImagesAreMedia(t *testing.T) {
	ln := fixtureLines(t, images)[1]
	rec := kiro.Convert(kiro.Source{Kind: kiro.SrcMessages}, 2, 0, []byte(ln), "79b8c44c-53be-43c6-9feb-203cc582bca7")
	if len(rec.Parts) != 3 || rec.Parts[0].Kind != sessiondata.PartText {
		t.Fatalf("parts %+v", rec.Parts)
	}
	for _, p := range rec.Parts[1:] {
		if p.Kind != sessiondata.PartMedia || p.Media != "image/png" || !bytes.Contains([]byte(ln), p.Data) {
			t.Fatalf("image part %+v", p)
		}
	}
	if rec.Run != "79b8c44c-53be-43c6-9feb-203cc582bca7" || rec.Trigger != model.TriggerExternal {
		t.Fatalf("run %q trigger %q", rec.Run, rec.Trigger)
	}
}

// The patterns are matched against the paths in a session's header. A session
// is left out only when every one of its paths is.
func TestMatcherJudgesWorkspacePaths(t *testing.T) {
	m := kiro.NewMatcher(nil, []string{"/private/tmp/**"})
	for paths, want := range map[string]bool{
		"/private/tmp/x":                   false,
		"/private/tmp":                     false,
		"/private/tmpfoo":                  true,
		"/private/tmp/x,/home/dev/project": true,
		"":                                 true,
	} {
		s := kiro.Session{}
		if paths != "" {
			s.Workspaces = strings.Split(paths, ",")
		}
		if got := m.Match(s); got != want {
			t.Errorf("%q: %v, want %v", paths, got, want)
		}
	}
	inc := kiro.NewMatcher([]string{"/home/dev/**"}, nil)
	if !inc.Match(kiro.Session{Workspaces: []string{"/home/dev/a"}}) || inc.Match(kiro.Session{Workspaces: []string{"/srv/a"}}) {
		t.Error("include")
	}
	// A Windows path matches in either spelling, on every host: the pattern is
	// written by hand, and the session may have been written on another host.
	for _, pat := range []string{`C:\work\private\**`, "C:/work/private/**"} {
		win := kiro.NewMatcher(nil, []string{pat})
		for ws, want := range map[string]bool{
			`C:\work\private\x`: false,
			"C:/work/private/x": false,
			`C:\work\public`:    true,
		} {
			if got := win.Match(kiro.Session{Workspaces: []string{ws}}); got != want {
				t.Errorf("%s against %s: %v, want %v", pat, ws, got, want)
			}
		}
	}
}

// fixtureLines is a fixture session's messages.jsonl, one string per line.
func fixtureLines(t *testing.T, id string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(fixtures, "sessions", "*", id, "messages.jsonl"))
	if err != nil || len(found) != 1 {
		t.Fatalf("%s: %v %v", id, found, err)
	}
	data, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func mkfile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestImagesTooLargeToLandAreLeftOut lands Kiro Crew's prompt with two
// images under a budget its record does not fit. The two images are left
// out, the sentence beside them lands, every file fits, and the prompt's step
// says too_large. The budget is the one measured to leave out the images
// alone: at 1,050 bytes the sentence goes too, at 1,100 one image stays.
func TestImagesTooLargeToLandAreLeftOut(t *testing.T) {
	const budget = 1075
	z := storage.NewZone(t.TempDir())
	r := &root{t: t, zone: z, col: kiro.New(fixtures, z, budget)}
	// A small budget takes more files than one pass lands.
	for i := 0; ; i++ {
		st, err := r.col.CollectAll(nil)
		if err != nil {
			t.Fatal(err)
		}
		if st.Complete() {
			break
		}
		if i > 10 {
			t.Fatal("collection does not finish")
		}
	}
	r.parse(images)
	files, err := storage.LandedFiles(z, images)
	if err != nil {
		t.Fatal(err)
	}
	media := 0
	for _, lf := range files {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > budget {
			t.Errorf("%s is %d bytes, over the budget of %d", filepath.Base(lf.Path), len(data), budget)
		}
		_, recs, err := sessiondata.All(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs {
			for _, p := range rec.Parts {
				switch {
				case p.State != model.ContentTooLarge:
				case p.Kind == sessiondata.PartMedia && p.Media == "image/png" && len(p.SHA256) == 64 && p.Bytes > 0 && len(p.Data) == 0:
					media++
				default:
					t.Errorf("%s left out a %s part", rec.ID, p.Kind)
				}
			}
		}
	}
	if media != 2 {
		t.Fatalf("left out %d image(s), want the two", media)
	}
	found := false
	for _, n := range nodes(r.doc(images)) {
		if n.State == model.ContentTooLarge {
			if n.Kind != model.KindMessageExternal || n.Text == "" {
				t.Fatalf("step %s %s says too_large with text %q; want the prompt, with its sentence", n.ID, n.Kind, n.Text)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no step says too_large")
	}
}
