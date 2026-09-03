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
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// parsed is a collected and parsed session, ready to assert against.
type parsed struct {
	zone  *storage.Zone
	src   string
	x     *transcript
	view  *sessionflow.View
	round *parse.Round
}

// build lands a transcript and parses it into a round chain.
func build(t *testing.T, fill func(x *transcript)) *parsed {
	t.Helper()
	src := t.TempDir()
	z := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)
	fill(x)
	x.Flush()

	col := claudecode.New(src, z, 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	p := &parsed{zone: z, src: src, x: x}
	p.reparse(t)
	return p
}

// reparse collects again and appends another round.
func (p *parsed) reparse(t *testing.T) {
	t.Helper()
	col := claudecode.New(p.src, p.zone, 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	r, err := parse.Session(p.zone, parse.Options{
		Conversation: p.x.session, Session: p.x.session,
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v, err := parse.View(p.zone.Root(), p.x.session)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	p.round, p.view = r, v
}

// kinds counts the folded view's nodes by kind.
func (p *parsed) kinds() map[string]int {
	out := map[string]int{}
	for _, n := range p.view.Nodes {
		out[n.Kind]++
	}
	return out
}

// relations counts the folded view's relations by type.
func (p *parsed) relations() map[string]int {
	out := map[string]int{}
	for _, r := range p.view.Relations {
		out[r.Type]++
	}
	return out
}

// talksOn returns the talks belonging to one stream.
func (p *parsed) talksOn(stream string) []*sessionflow.Node {
	var out []*sessionflow.Node
	for _, n := range p.view.Nodes {
		if n.Kind == model.KindTalk && n.Stream == stream {
			out = append(out, n)
		}
	}
	return out
}

// nodesOf returns every node of one kind.
func (p *parsed) nodesOf(kind string) []*sessionflow.Node {
	var out []*sessionflow.Node
	for _, n := range p.view.Nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// TestSessionAssemblesIntoConversation is the end-to-end shape: landed records
// in, a conversation structure out, with every level of the hierarchy present.
func TestSessionAssemblesIntoConversation(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("first thing", false)
		x.AddTurn("second thing, delegate it", true)
		x.AddTurn("third thing", false)
	})

	k := p.kinds()
	for _, want := range []struct {
		kind string
		n    int
	}{
		{model.KindSession, 1},
		{model.KindStream, 2}, // main plus the one child agent
		{model.KindTalk, 4},   // three on the parent lineage, one for the child
		{model.KindEpoch, 2},  // one per stream, no context reset
	} {
		if k[want.kind] != want.n {
			t.Errorf("%s: got %d, want %d", want.kind, k[want.kind], want.n)
		}
	}
	// Three human turns. The cycle that ran when the child finished is not a
	// fourth, and the child's own talk belongs to the child.
	if n := len(p.talksOn("main")); n != 3 {
		t.Errorf("talks on the parent lineage: %d, want 3", n)
	}
	if k[model.KindLLMCall] == 0 || k[model.KindTool] == 0 {
		t.Errorf("no provider calls or tools: %v", k)
	}
	if p.round.Number != 1 {
		t.Errorf("first parse wrote round %d", p.round.Number)
	}
}

// A background agent finishing and the parent resuming is a new prompt cycle,
// but nobody said anything - it is the same interaction continuing. Splitting on
// the cycle would show a reader two turns where there was one.
func TestDelegationStaysInsideOneTalk(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("delegate this", true) })

	main := p.talksOn("main")
	if len(main) != 1 {
		t.Fatalf("got %d talks on the parent lineage, want 1", len(main))
	}
	// Two prompt cycles, one talk: the human turn and the notification that the
	// child finished.
	if runs := p.view.Children(main[0].ID); len(runs) != 2 {
		t.Errorf("got %d runs in the talk, want 2 (the human cycle and the notification cycle)", len(runs))
	}
	k := p.kinds()
	if k[model.KindAgentLaunchAck] != 1 {
		t.Errorf("launch acknowledgement: %d, want 1", k[model.KindAgentLaunchAck])
	}
	if k[model.KindRuntimeNotification] != 1 {
		t.Errorf("completion notification: %d, want 1", k[model.KindRuntimeNotification])
	}
	if k[model.KindAgentOutput] != 1 {
		t.Errorf("child output: %d, want 1", k[model.KindAgentOutput])
	}
}

// The child's output belongs to the child. The parent references it and never
// absorbs it, which is what stops a rendered conversation showing the same work
// twice.
func TestChildOutputIsOwnedByTheChildStream(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("delegate this", true) })

	out := p.nodesOf(model.KindAgentOutput)
	if len(out) != 1 {
		t.Fatalf("got %d outputs, want 1", len(out))
	}
	if out[0].Stream == "main" {
		t.Fatal("the child's output was placed on the parent lineage")
	}
	// The parent reaches it through a relation, never through containment.
	if r := p.relations(); r[model.RelStarts] != 1 {
		t.Errorf("spawn relations: %d, want 1", r[model.RelStarts])
	}
	for _, n := range p.view.Nodes {
		if n.Stream == "main" && strings.HasPrefix(n.ID, "output/") {
			t.Errorf("child output %s is contained by the parent lineage", n.ID)
		}
	}
}

// A tool use is one step carrying the request and the result, not two siblings.
func TestToolIsOneStepWithBothSides(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("run something", false) })

	tools := p.nodesOf(model.KindTool)
	if len(tools) != 1 {
		t.Fatalf("got %d tool steps, want 1", len(tools))
	}
	tool := tools[0]
	if len(tool.Refs) != 2 {
		t.Fatalf("tool references %d records, want 2 (the request and the result)", len(tool.Refs))
	}
	a := string(tool.Attrs)
	if !strings.Contains(a, `"result":"available"`) {
		t.Errorf("result state missing: %s", a)
	}
	// The runtime reports no execution timing apart from record timestamps, so
	// it must be stated unavailable rather than guessed from the gap.
	if !strings.Contains(a, `"timing":"unavailable"`) {
		t.Errorf("timing should be unavailable, not inferred: %s", a)
	}
}

// A tool whose result never arrived is kept with an unavailable result. It is
// not dropped, and it is not given an empty one.
func TestUnfinishedToolIsKeptAndReported(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("start something long", false)
		x.AddOrphanTool()
	})

	var orphan *sessionflow.Node
	for _, n := range p.nodesOf(model.KindTool) {
		if strings.Contains(string(n.Attrs), `"result":"unavailable"`) {
			orphan = n
		}
	}
	if orphan == nil {
		t.Fatal("the unfinished tool was dropped or given a result it never had")
	}
	if len(orphan.Refs) != 1 {
		t.Errorf("an unfinished tool references %d records, want 1", len(orphan.Refs))
	}
	var found bool
	for _, u := range p.view.OpenUnresolved() {
		if u.Kind == "tool_result" {
			found = true
		}
	}
	if !found {
		t.Error("the missing result was not recorded as unresolved")
	}
}

// An assistant-role record the client produced must never be counted as agent
// output, and must not be dropped either - it marks where a conversation broke.
func TestSyntheticIsSeparatedFromAgentOutput(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("do the thing", false)
		x.AddSynthetic("API Error: connection reset")
	})

	k := p.kinds()
	if k[model.KindMessageSynthetic] != 1 {
		t.Fatalf("synthetic messages: %d, want 1", k[model.KindMessageSynthetic])
	}
	if model.CountsAsAgentOutput(model.KindMessageSynthetic) {
		t.Fatal("the model counts a synthetic record as agent output")
	}
}

// Human input that exists only as an attachment must be recovered, and a
// completion blob queued the same way must not be read as something a person
// typed.
func TestAttachmentOnlyHumanInputIsRecovered(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("do the thing", false)
		x.AddQueuedPrompt("also check the tests", "prompt")
		x.AddQueuedPrompt("<task-notification>...</task-notification>", "task-notification")
		x.AddInjection("todo_reminder", "your todo list is empty")
	})

	k := p.kinds()
	// The human turn plus the queued prompt. The queued notification is NOT one:
	// it was queued the same way and looks the same apart from its command mode.
	if k[model.KindMessageExternal] != 2 {
		t.Errorf("external input steps: %d, want 2", k[model.KindMessageExternal])
	}
	// The reminder, and the queued notification. Neither is something a person
	// said, and neither is dropped.
	if k[model.KindContextInjection] != 2 {
		t.Errorf("injected context steps: %d, want 2", k[model.KindContextInjection])
	}
}

// A context reset is taken only from the explicit boundary, and the summary is
// paired with it through containment - never by timestamp, which runs backwards
// here on purpose.
func TestContextResetCutsAnEpoch(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("before the reset", false)
		x.AddCompaction()
		x.AddTurn("after the reset", false)
	})

	k := p.kinds()
	if k[model.KindEpochBoundary] != 1 {
		t.Fatalf("epoch boundaries: %d, want 1", k[model.KindEpochBoundary])
	}
	if k[model.KindEpochSummary] != 1 {
		t.Fatalf("epoch summaries: %d, want 1", k[model.KindEpochSummary])
	}
	var mainEpochs int
	for _, e := range p.nodesOf(model.KindEpoch) {
		if e.Stream == "main" {
			mainEpochs++
		}
	}
	if mainEpochs != 2 {
		t.Errorf("the parent lineage has %d epochs, want 2", mainEpochs)
	}
	r := p.relations()
	if r[model.RelFollows] != 1 {
		t.Errorf("epoch succession: %d, want 1", r[model.RelFollows])
	}
	if r[model.RelSummarizes] != 1 {
		t.Errorf("summary pairing: %d, want 1 (paired by containment, not timestamp)", r[model.RelSummarizes])
	}
}

// A fork is announced only by the parent's result, with no separate pointer to
// the call, so the tool id has to come from the result's own content block.
func TestSkillForkResolvesFromTheResultBlock(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("use the skill", false)
		x.AddSkillFork()
	})

	if r := p.relations(); r[model.RelStarts] != 1 {
		t.Fatalf("spawn relations: %d, want 1", r[model.RelStarts])
	}
	for _, u := range p.view.OpenUnresolved() {
		if u.Kind == "spawn_of_child" {
			t.Errorf("the forked child was left without a parent: %s", u.Reason)
		}
	}
}

// A tool result's runtime enrichment is sometimes a bare string rather than an
// object. Decoding it into a struct fails, and because the failure covers the
// whole record every identifier on it is lost - the tool join included.
func TestStringToolResultDoesNotLoseTheRecord(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("read a file", false)
		x.AddStringToolResult()
	})

	var joined int
	for _, n := range p.nodesOf(model.KindTool) {
		if strings.Contains(string(n.Attrs), `"result":"available"`) {
			joined++
		}
	}
	if joined != 2 {
		t.Errorf("%d tools joined to their results, want 2 - a string enrichment broke the record", joined)
	}
}

// Records the runtime re-emits before a context reset carry the same ids as the
// originals. The first copy wins: the later one has a rewritten prompt cycle and
// less captured output, so keeping it would both move records and lose data.
func TestReEmittedRecordsDoNotDuplicateTheConversation(t *testing.T) {
	clean := build(t, func(x *transcript) {
		x.AddTurn("one", false)
		x.AddTurn("two", false)
	})
	replayed := build(t, func(x *transcript) {
		x.AddTurn("one", false)
		x.AddTurn("two", false)
		x.AddReplay(5)
	})

	a, b := clean.kinds(), replayed.kinds()
	for _, kind := range []string{model.KindTalk, model.KindRun, model.KindLLMCall, model.KindTool} {
		if a[kind] != b[kind] {
			t.Errorf("%s: %d without re-emitted records, %d with them", kind, a[kind], b[kind])
		}
	}
}

// Every attachment carries something: a person's typed command, or material the
// harness injected. Classifying by a list of known types loses whatever is not
// on the list, and the list is always behind - 13 real attachment types were
// dropped this way, while 5 types on the list appeared nowhere in the corpus.
func TestUnknownAttachmentIsStillCarried(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("do the thing", false)
		x.AddInjection("mcp_instructions_delta", "server instructions")
		x.AddInjection("a_type_that_did_not_exist_when_this_was_written", "something new")
	})

	if n := p.kinds()[model.KindContextInjection]; n != 2 {
		t.Errorf("injected context steps: %d, want 2 - an unrecognised attachment was dropped", n)
	}
}

// A child started by a workflow is reached through the batch it belongs to: the
// parent's launch result names the batch, and the batch's journal names every
// child in it. Nothing else connects them - a workflow child's own transcript
// says nothing about who started it.
//
// This is by far the largest source of child-to-parent joins, so a break here
// is quiet and expensive: the children still assemble, they just have no parent.
func TestWorkflowChildrenResolveThroughTheirBatch(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("run the workflow", false)
		x.AddWorkflow(3)
	})

	if r := p.relations(); r[model.RelStarts] != 3 {
		t.Errorf("start relations: %d, want 3 - one per workflow child", r[model.RelStarts])
	}
	for _, u := range p.view.OpenUnresolved() {
		if u.Kind == "spawn_of_child" {
			t.Errorf("a workflow child was left with no parent: %s", u.RefID)
		}
	}
}

// Every system record becomes a step, including subtypes nobody has seen yet.
//
// This is a category that was silently dropped once already: two model kinds -
// a turn's duration and a command invoked in-line - were declared, documented,
// and never produced, while 489 records that carried them did nothing. A list
// of known subtypes is what caused it, so the default is a notice rather than
// nothing.
func TestEverySystemRecordBecomesAStep(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("do the thing", false)
		x.AddSystem("turn_duration", map[string]any{"durationMs": 12345, "messageCount": 9})
		x.AddSystem("local_command", map[string]any{"content": "<local-command-stdout></local-command-stdout>"})
		x.AddSystem("a_subtype_from_the_future", map[string]any{"content": "something new"})
	})

	k := p.kinds()
	for _, want := range []struct {
		kind string
		n    int
	}{
		{model.KindTurnDuration, 1},
		{model.KindControlCommand, 1},
		{model.KindControlNotice, 1},
	} {
		if k[want.kind] != want.n {
			t.Errorf("%s: %d, want %d", want.kind, k[want.kind], want.n)
		}
	}
}
