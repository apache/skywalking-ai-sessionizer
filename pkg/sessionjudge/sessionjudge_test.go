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

package sessionjudge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func header() Header {
	return Header{
		Conversation: "c1", Session: "c1", Run: 1, Stage: StageTargets,
		Head: Head{Round: 3, Digest: "abc"}, Parser: "v1", Policy: "v1+idle=10m0s",
		Evaluator: Evaluator{Name: "claude", Kind: EvaluatorCLI, Program: "claude"},
		Started:   "2026-10-20T10:00:00Z", Finished: "2026-10-20T10:05:00Z", Status: StatusComplete,
	}
}

func write(t *testing.T) ([]byte, string) {
	t.Helper()
	w, err := NewWriter(header())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&Target{ID: "t1", Statement: "fix <login> & keep the API", Status: TargetClosed,
		Talks: []string{"talk/main/1"}, Cite: []Cite{{Node: "msg/1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&Input{Node: "msg/1", Talk: "talk/main/1", Actions: []Action{{Target: "t1", Action: ActionCreate}},
		Relations: []Relation{{Kind: RelationNew, Goal: "g1", Request: "fix <login>", Statement: "fix the login"}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(goal()); err != nil {
		t.Fatal(err)
	}
	data, digest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return data, digest
}

func goal() *Goal {
	return &Goal{ID: "g1", Statement: "fix the login", Members: []string{"t1"}, Main: "t1", MainReason: "the only member",
		Evolution: []Evolution{{Input: "msg/1", Statement: "fix the login"}}, Cite: []Cite{{Node: "msg/1"}}}
}

// A file reads back as it was written, with its text unescaped.
func TestAStageFileReadsBack(t *testing.T) {
	data, digest := write(t)
	if !bytes.Contains(data, []byte("fix <login> & keep")) {
		t.Fatalf("text was escaped: %s", data)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if f.Commit.Digest != digest || len(f.Targets) != 1 || len(f.Inputs) != 1 || f.Targets[0].Statement != "fix <login> & keep the API" {
		t.Fatalf("read back %+v", f)
	}
	if f.Header.Schema != Schema || f.Header.T != FrameHeader || f.Targets[0].T != FrameTarget {
		t.Fatalf("frame types not set: %+v", f.Header)
	}
	// A goal, and the relations of an input, read back whole.
	want := goal()
	want.T = FrameGoal
	if len(f.Goals) != 1 || !reflect.DeepEqual(f.Goals[0], *want) {
		t.Fatalf("goals %+v", f.Goals)
	}
	if r := f.Inputs[0].Relations; len(r) != 1 || r[0] != (Relation{Kind: RelationNew, Goal: "g1", Request: "fix <login>", Statement: "fix the login"}) {
		t.Fatalf("relations %+v", r)
	}
}

// Any change to a line before the commit fails the digest, the header
// included, and a dropped frame fails the counts.
func TestATamperedFileIsRefused(t *testing.T) {
	data, _ := write(t)
	lines := strings.SplitAfter(string(data), "\n")
	cases := map[string]string{
		"header":    strings.Replace(string(data), `"run":1`, `"run":2`, 1),
		"frame":     strings.Replace(string(data), "keep the API", "keep the APP", 1),
		"truncated": strings.Join(lines[:len(lines)-2], ""),
		"no commit": strings.Join(lines[:3], ""),
		"after":     string(data) + `{"t":"target","id":"t2"}` + "\n",
		"no header": strings.Join(lines[1:], ""),
	}
	for name, body := range cases {
		if _, err := Read(strings.NewReader(body)); err == nil {
			t.Errorf("%s: a changed file was accepted", name)
		}
	}
	// A dropped frame with a digest recomputed over what remains still fails
	// on the counts.
	w, _ := NewWriter(header())
	_ = w.Write(&Target{ID: "t1", Statement: "s", Status: TargetOpen, Talks: []string{}, Cite: []Cite{}})
	full, _, _ := w.Close()
	parts := strings.SplitAfter(string(full), "\n")
	commit := strings.Replace(parts[2], `"target":1`, `"target":2`, 1)
	if _, err := Read(strings.NewReader(parts[0] + parts[1] + commit)); err == nil {
		t.Error("counts that do not match the frames were accepted")
	}
}

// A file is read only with one header, on its first line. A digest and
// counts made again over the changed lines, as a writer would have made them,
// leave the header's place the one thing that refuses the file.
func TestAHeaderIsFirstAndOnlyFirst(t *testing.T) {
	data, _ := write(t)
	lines := strings.SplitAfter(string(data), "\n")
	if _, err := Read(strings.NewReader(rehash(lines[0] + lines[1] + lines[2]))); err != nil {
		t.Fatalf("the file as written, closed again, was refused: %v", err)
	}
	cases := map[string]string{
		"no header":    lines[1] + lines[2],
		"header later": lines[1] + lines[0] + lines[2],
		"two headers":  lines[0] + lines[1] + lines[0] + lines[2],
	}
	for name, body := range cases {
		if _, err := Read(strings.NewReader(rehash(body))); err == nil || !strings.Contains(err.Error(), "a header must be the first line and only there") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// What a correct writer never writes is refused when read, each by its own
// check: every file here has a digest and counts that match its lines.
func TestWhatNoWriterWritesIsRefused(t *testing.T) {
	data, _ := write(t)
	lines := strings.SplitAfter(string(data), "\n")
	// The commit is the last line, before the empty string after it.
	commit := len(lines) - 2
	cases := map[string]struct{ body, err string }{
		// A header alone has no frame to count, so only the missing commit
		// says the file was cut short.
		"header only": {lines[0], "no commit"},
		// A second commit, closing the first, is content after the commit.
		"two commits": {rehash(string(data)), "content after the commit"},
		// The counts name exactly the frames there are, no more.
		"extra count": {strings.Join(lines[:commit], "") + strings.Replace(lines[commit], `"counts":{`, `"counts":{"label":0,`, 1), "counts"},
		// A line cut off after the commit is a file cut short too.
		"no newline": {string(data) + `{"t":"target","id":"t2"}`, "last line has no newline"},
		"empty":      {"", "empty file"},
		"not json":   {rehash(lines[0] + `{"t":"target",` + "\n" + lines[1] + lines[2]), "undecodable frame"},
		// A frame of a type this reader does not know may mean anything.
		"unknown frame": {rehash(lines[0] + `{"t":"mystery"}` + "\n" + lines[1] + lines[2]), `unknown frame "mystery"`},
		// A frame is read as its type says, or not at all.
		"wrong type": {rehash(lines[0] + strings.Replace(lines[1], `"id":"t1"`, `"id":1`, 1) + lines[2]), "cannot unmarshal"},
		// A stage frame that does not decode says so. Its digest is then
		// empty, and a reader told only that the digest is wrong would look
		// for the wrong fault.
		"stage of the wrong type": {rehash(lines[0] + `{"t":"stage","stage":"ledger","digest":7}` + "\n" + lines[1] + lines[2]), "cannot unmarshal"},
	}
	for name, tc := range cases {
		if _, err := Read(strings.NewReader(tc.body)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A file that cannot be read is an error of its own, never taken for a file
// cut short: the reader returns what its source said.
func TestAReadErrorIsReturned(t *testing.T) {
	data, _ := write(t)
	broken := errors.New("disk gone")
	header := strings.SplitAfter(string(data), "\n")[0]
	_, err := Read(io.MultiReader(strings.NewReader(header), iotest.ErrReader(broken)))
	if !errors.Is(err, broken) {
		t.Fatalf("read %v", err)
	}
}

// A reader checks every header requirement on its own, as a writer does.
func TestEveryHeaderRequirementIsRead(t *testing.T) {
	data, _ := write(t)
	lines := strings.SplitAfter(string(data), "\n")
	for _, tc := range []struct{ from, to, err string }{
		{`"schema":"sj/1"`, `"schema":"sj/2"`, "schema"},
		{`"conversation":"c1"`, `"conversation":""`, "no conversation or session"},
		{`"session":"c1"`, `"session":""`, "no conversation or session"},
		{`"run":1`, `"run":0`, "runs count from 1"},
		{`"stage":"targets"`, `"stage":"summary"`, `unknown stage "summary"`},
		{`"round":3`, `"round":0`, "no head round"},
		{`"digest":"abc"`, `"digest":""`, "no head round"},
		{`"name":"claude"`, `"name":""`, "no evaluator"},
		{`"kind":"cli"`, `"kind":""`, "no evaluator"},
		{`"status":"complete"`, `"status":"done"`, "unknown status"},
		{`"started":"2026-10-20T10:00:00Z"`, `"started":""`, "no start or finish"},
		{`"finished":"2026-10-20T10:05:00Z"`, `"finished":""`, "no start or finish"},
	} {
		if !strings.Contains(lines[0], tc.from) {
			t.Fatalf("the header has no %s", tc.from)
		}
		body := rehash(strings.Replace(lines[0], tc.from, tc.to, 1) + lines[1] + lines[2])
		if _, err := Read(strings.NewReader(body)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %v", tc.to, err)
		}
	}
}

// A header missing what every header must carry is refused when written.
func TestAnIncompleteHeaderIsRefused(t *testing.T) {
	for name, change := range map[string]func(*Header){
		"stage":     func(h *Header) { h.Stage = "summary" },
		"run":       func(h *Header) { h.Run = 0 },
		"head":      func(h *Header) { h.Head = Head{} },
		"evaluator": func(h *Header) { h.Evaluator = Evaluator{} },
		"status":    func(h *Header) { h.Status = "done" },
		"times":     func(h *Header) { h.Finished = "" },
	} {
		h := header()
		change(&h)
		if _, err := NewWriter(h); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A key a later writer adds is passed over, so an earlier reader still reads
// the file.
func TestAnUnknownKeyIsPassedOver(t *testing.T) {
	data, _ := write(t)
	lines := strings.SplitAfter(string(data), "\n")
	lines[1] = strings.Replace(lines[1], `{"t":"target",`, `{"t":"target","later":1,`, 1)
	body := strings.Join(lines[:len(lines)-2], "")
	f, err := Read(strings.NewReader(rehash(body)))
	if err != nil {
		t.Fatalf("an added key stopped the reader: %v", err)
	}
	if f.Targets[0].ID != "t1" {
		t.Fatalf("read %+v", f.Targets)
	}
}

// rehash closes a body with a fresh commit, as a writer would have: a digest
// over every line, and a count of every frame but a header and a commit,
// wherever the lines put them.
func rehash(body string) string {
	f := &Writer{counts: map[string]int{}}
	f.buf.WriteString(body)
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		var probe struct {
			T FrameType `json:"t"`
		}
		_ = json.Unmarshal([]byte(line), &probe)
		if probe.T != FrameHeader && probe.T != FrameCommit {
			f.counts[string(probe.T)]++
		}
	}
	f.enc = newEncoder(&f.buf)
	data, _, _ := f.Close()
	return string(data)
}

// A stage frame must name its file by a whole digest, in lowercase hex: a
// close listing any other is refused when read, before anything acts on it.
func TestAStageDigestMustBeWhole(t *testing.T) {
	// One character short or one too many is no SHA-256, though every
	// character is lowercase hex.
	for _, digest := range []string{"x", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("a", 64)} {
		h := header()
		h.Stage = StageClose
		w, _ := NewWriter(h)
		_ = w.Write(&Stage{Stage: StageLedger, File: "s1-ledger-x.sj", Digest: digest, Status: StatusComplete, Evaluator: "asz"})
		data, _, _ := w.Close()
		_, err := Read(bytes.NewReader(data))
		if whole := digest == strings.Repeat("a", 64); whole != (err == nil) {
			t.Errorf("digest %s: %v", digest, err)
		}
	}
}

// A sub-task stage reads back with its decisions, counted like any frame.
func TestASubtaskStageReadsBack(t *testing.T) {
	h := Header{Schema: Schema, Conversation: "c", Session: "c", Run: 1, Stage: StageSubtasks,
		Head: Head{Round: 1, Digest: strings.Repeat("a", 64)}, Evaluator: Evaluator{Name: "e", Kind: EvaluatorCLI, Program: "claude"},
		Started: "2026-10-20T00:00:00Z", Finished: "2026-10-20T00:01:00Z", Status: StatusComplete}
	w, err := NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&Subtask{ID: "stream/a", Parent: "talk/main/1", StartedBy: "tool/x", Outcome: LevelAchieved,
		Report: ReportAccurate, Use: UseUsed, Dispatch: DispatchReasonable, Work: WorkUseful, Cite: []Cite{{Node: "tool/y"}}}); err != nil {
		t.Fatal(err)
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Subtasks) != 1 || f.Subtasks[0].Report != ReportAccurate || f.Commit.Counts["subtask"] != 1 {
		t.Fatalf("read back %+v counts %v", f.Subtasks, f.Commit.Counts)
	}
}

// An achievement reads back with the history of its result: each event,
// each contradiction tied to the delivery it disproves, each round of
// rework, what each criterion's steps show, the account and the findings.
func TestAnAchievementReadsBackWithItsEvents(t *testing.T) {
	h := header()
	h.Stage = StageAchievement
	w, err := NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	want := Achievement{T: FrameAchievement, Target: "t1", Level: LevelAchieved, Basis: BasisObserved, Cite: []Cite{{Node: "tool/3"}},
		Events: []Event{{ID: "t1.e1", Kind: EventDelivered, Artifact: ArtifactRevision, Text: "committed", Cite: []Cite{{Node: "tool/1"}}},
			{ID: "t1.e2", Kind: EventUndone, Artifact: ArtifactRevision, Text: "reverted", Cite: []Cite{{Node: "tool/2"}}}},
		Contradictions: []Contradiction{{Event: "t1.e1", Criteria: []string{"t1.c2"}, Text: "the test failed", Cite: []Cite{{Node: "tool/2"}}},
			{Event: "t1.e1", Conclusion: true, Text: "the cause was another", Cite: []Cite{{Node: "msg/2"}}}},
		Rework: []Rework{{ID: "t1.r1", Attempt: []Cite{{Node: "tool/1"}}, Trigger: TriggerPerson, TriggerCite: []Cite{{Node: "msg/2"}},
			Cause: ReworkAgentError, Redo: []Cite{{Node: "tool/3"}}, From: "msg/2", To: "tool/3", Finished: true, Note: "fixed again"}},
		Criteria: []Check{{ID: "t1.c1", Text: "it builds", State: CheckUnknown, Cite: []Cite{{Node: "tool/3"}}, Reason: "nothing it cites shows it",
			GivenReason: "the build passed", Observations: []Observation{{Node: "tool/3", Ref: &Ref{Seq: 2, Row: 7}, Excerpt: "ok", Status: ObservedReturned,
				Interpretation: "the build passed"}}}},
		Account: &Account{Answer: &Answer{Asked: Said{Text: "why", Cite: []Cite{{Node: "msg/1"}}}, Checked: []Tried{{What: "the log", Found: "a timeout",
			Limits: "one run", Cite: []Cite{{Node: "tool/1"}}}}, Answered: Said{Text: "the clock", Cite: []Cite{{Node: "reply/1"}}}},
			Files: []FileGroup{{Area: "internal/judge", Category: "code", Summary: "goals", Operations: []string{"o1", "o3"}}},
			Remote: []Remote{{System: "GitHub", Operation: "merge", Via: "gh", Request: Said{Text: "merge 87", Cite: []Cite{{Node: "tool/4"}}},
				Confirmed: &Said{Text: "merged", Cite: []Cite{{Node: "tool/5"}}}}}},
		Findings: []Finding{{At: "tool/1", Ref: &Ref{Seq: 2, Row: 3}, Problem: "it guessed the cause", Explanation: "it read no log",
			Improvement: "read the log first", Cite: []Cite{{Node: "msg/2"}}}}}
	if err := w.Write(&want); err != nil {
		t.Fatal(err)
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Achievements) != 1 || !reflect.DeepEqual(f.Achievements[0], want) {
		t.Fatalf("read back %+v", f.Achievements)
	}
}

// A close reads back with how each target ended and how each goal did.
func TestACloseReadsBackWithItsReport(t *testing.T) {
	h := header()
	h.Stage = StageClose
	half, one, zero := 0.5, 1.0, 0.0
	h.Summary = &Summary{Targets: 1, Score: &Score{Achievement: &half, Final: &zero, Judged: 1},
		Goals: []GoalOutcome{{Goal: "g1", Main: "t1", Worst: ClassContradicted, ScoreMain: &zero, ScoreMean: &zero,
			First: &Finding{Target: "t1", Source: SourceASZ, At: "tool/1", Problem: "criterion t1.c2 was not met"}}},
		UnassignedOperations: []string{"o3"}}
	w, err := NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	want := Outcome{T: FrameOutcome, Target: "t1", Statement: "fix it", Status: TargetClosed, Level: LevelPartly, Class: ClassContradicted,
		Standing: []string{"t1.e3"}, ScoreRecord: &half, ScoreFinal: &zero, Corrections: []string{"msg/2"},
		Rework:     []ReworkCost{{Work: WorkRepair, Cause: CauseMain, Cost: Amount{Money: &one}}, {Work: WorkNotReviewed, Cost: Amount{Money: &one}}},
		Operations: []OperationShare{{ID: "o1"}, {ID: "o2", Share: &half}}}
	if err := w.Write(&want); err != nil {
		t.Fatal(err)
	}
	asz := Finding{T: FrameFinding, Target: "t1", Source: SourceASZ, At: "tool/1", Problem: "criterion t1.c2 was not met", Cite: []Cite{{Node: "tool/1"}}}
	if err := w.Write(&asz); err != nil {
		t.Fatal(err)
	}
	two := 2
	op := Operation{T: FrameOperation, ID: "o1", Step: "tool/1", Record: "c1", Workspace: "/w", Path: "a.go", Operation: "modify",
		Source: SourceChangeRecord, Added: &two, Ref: &Ref{Seq: 3, Row: 4}}
	if err := w.Write(&op); err != nil {
		t.Fatal(err)
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Outcomes) != 1 || !reflect.DeepEqual(f.Outcomes[0], want) || len(f.Findings) != 1 || !reflect.DeepEqual(f.Findings[0], asz) ||
		len(f.Operations) != 1 || !reflect.DeepEqual(f.Operations[0], op) {
		t.Fatalf("read back %+v %+v %+v", f.Outcomes, f.Findings, f.Operations)
	}
	if !reflect.DeepEqual(f.Header.Summary, h.Summary) {
		t.Fatalf("summary %+v", f.Header.Summary)
	}
}

// A regroup reads back with its label file, the person's goals, where each
// target went and how the two groupings compared; it is no stage of a run,
// yet its stage is known.
func TestARegroupReadsBack(t *testing.T) {
	h := header()
	h.Stage = StageRegroup
	h.Labels = &LabelFile{File: "labels.yaml", SHA256: strings.Repeat("b", 64), Version: 3}
	w, err := NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	goal := LabelGoal{T: FrameLabelGoal, ID: "P1", Statement: "build and test it", Members: []string{"t1", "t2"}, Main: "t1", Judge: []string{"g1", "g2"},
		Class: CompareSplit, Worked: true, Note: "both serve it", Cite: []Cite{{Node: "msg/2"}}}
	missed := LabelGoal{T: FrameLabelGoal, ID: "P2", Statement: "ship it", Members: []string{}, Judge: []string{}, Class: CompareMissed, Worked: true,
		Cite: []Cite{{Node: "msg/3"}}}
	placed := Placement{T: FramePlacement, Target: "t1", Goal: "P1", Note: "asked first", Cite: []Cite{{Node: "msg/1"}}}
	unknown := Placement{T: FramePlacement, Target: "t3", Unknown: true, Cite: []Cite{}}
	agree := Agreement{T: FrameAgreement, Goals: 2, Classes: map[string]int{CompareSplit: 1, CompareMissed: 1}, Extra: []string{"t4"}, Unknown: []string{"t3"}}
	text := LabelText{T: FrameLabelText, From: 1, Text: "goals:\n  - build and test it\n  - ship it\n"}
	for _, f := range []any{&text, &goal, &missed, &placed, &unknown, &agree} {
		if err := w.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Header.Labels, h.Labels) || !reflect.DeepEqual(f.LabelGoals, []LabelGoal{goal, missed}) ||
		!reflect.DeepEqual(f.Placements, []Placement{placed, unknown}) || !reflect.DeepEqual(f.Agreements, []Agreement{agree}) ||
		!reflect.DeepEqual(f.LabelTexts, []LabelText{text}) {
		t.Fatalf("read back %+v %+v %+v %+v %+v", f.Header.Labels, f.LabelGoals, f.Placements, f.Agreements, f.LabelTexts)
	}
	// A stage no run takes and no regroup is is refused.
	h.Stage = "relabel"
	if _, err := NewWriter(h); err == nil {
		t.Fatal("an unknown stage was written")
	}
}

// The reference and divergence stages read back with their sources, their
// conclusions, and how the conversation compared.
func TestTheReferenceStagesReadBack(t *testing.T) {
	h := header()
	h.Stage = StageReference
	w, err := NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	src := Source{T: FrameSource, ID: "R1", Source: "https://x/OPS-1", Captured: "2026-10-23T09:00:00Z", Method: MethodHTTP, Outcome: SourceSaved,
		ContentType: "text/html", Bytes: 120, SHA256: strings.Repeat("d", 64), Cut: true, Revision: `"v2"`, Published: "Wed, 21 Oct 2026 07:28:00 GMT",
		Author: "ops", Text: &Extracted{Bytes: 40, SHA256: strings.Repeat("e", 64)}, Lines: 3}
	text := SourceText{T: FrameSourceText, Source: "R1", From: 1, Text: "a\nb\nc\n"}
	k := Conclusion{T: FrameConclusion, ID: "r1", Source: "R1", Kind: ConclusionFinal, Statement: "b", Expectation: ExpectRequired, Scope: []string{"msg/1"},
		Context: []SourceCite{{Source: "R1", Lines: [2]int{2, 2}, Note: "the cause"}}, BuildsOn: []string{"r0"}, Support: SupportSaved, Author: "ops",
		Revision: "0.4.0", Environment: "prod", Conflicts: []string{"r2"}, Ambiguity: "which node", NotCovered: "the fix", Unbound: []string{"msg/9"}}
	for _, f := range []any{&src, &text, &k} {
		if err := w.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil || !reflect.DeepEqual(f.Sources, []Source{src}) || !reflect.DeepEqual(f.SourceTexts, []SourceText{text}) || !reflect.DeepEqual(f.Conclusions, []Conclusion{k}) {
		t.Fatalf("reference read back %v %+v %+v %+v", err, f.Sources, f.SourceTexts, f.Conclusions)
	}
	h.Stage = StageDivergence
	w, err = NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	d := Divergence{T: FrameDivergence, Conclusion: "r1", Verdict: VerdictContradicted, Target: "t1", Event: "t1.e2", Criteria: []string{"t1.c1"},
		Note: "n", Cite: []Cite{{Node: "msg/2"}}}
	rb := Rollback{T: FrameRollback, Conclusion: "r1", Target: "t1", Event: "t1.e2", Criteria: []string{"t1.c2"}, Note: "rolled back", Cite: []Cite{{Node: "tool/1"}}}
	fd := Finding{T: FrameFinding, Target: "t1", Source: StageDivergence, Conclusion: "r1", Cause: CauseNoClarification, Asserted: true, At: "msg/1",
		Problem: "it never asked", Cite: []Cite{}}
	for _, f := range []any{&d, &rb, &fd} {
		if err := w.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	data, _, err = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err = Read(bytes.NewReader(data))
	if err != nil || !reflect.DeepEqual(f.Divergences, []Divergence{d}) || !reflect.DeepEqual(f.Rollbacks, []Rollback{rb}) || !reflect.DeepEqual(f.Findings, []Finding{fd}) {
		t.Fatalf("divergence read back %v %+v %+v %+v", err, f.Divergences, f.Rollbacks, f.Findings)
	}
}

// The efficiency stage's findings are frames of their own beside its labels,
// each naming its target.
func TestAFindingFrameReadsBack(t *testing.T) {
	h := header()
	h.Stage = StageEfficiency
	w, err := NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	want := Finding{T: FrameFinding, Target: "t1", At: "tool/2", Problem: "the build ran twice", Improvement: "reuse the first", Cite: []Cite{{Node: "tool/2"}}}
	if err := w.Write(&Label{Target: "t1", Talk: "talk/1", Label: WorkRepeated, Cite: []Cite{{Node: "tool/2"}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&want); err != nil {
		t.Fatal(err)
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Labels) != 1 || len(f.Findings) != 1 || !reflect.DeepEqual(f.Findings[0], want) || f.Commit.Counts["finding"] != 1 {
		t.Fatalf("read back %+v %+v counts %v", f.Labels, f.Findings, f.Commit.Counts)
	}
}

// The commit is the last line of a file. Once a writer has closed it, a
// frame written or a second close is an error, and the bytes the close gave
// stay the whole file.
func TestAClosedFileTakesNothingMore(t *testing.T) {
	w, err := NewWriter(header())
	if err != nil {
		t.Fatal(err)
	}
	data, digest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	kept := append([]byte{}, data...)
	if err := w.Write(&Target{ID: "t2", Statement: "late", Status: TargetOpen, Talks: []string{}, Cite: []Cite{}}); err == nil ||
		!strings.Contains(err.Error(), "already closed") {
		t.Errorf("a frame after the commit: %v", err)
	}
	if again, d, err := w.Close(); err == nil || !strings.Contains(err.Error(), "already closed") || again != nil || d != "" {
		t.Errorf("a second close gave %d bytes, digest %q, error %v", len(again), d, err)
	}
	f, err := Read(bytes.NewReader(kept))
	if err != nil || !bytes.Equal(data, kept) || f.Commit.Digest != digest || len(f.Targets) != 0 {
		t.Fatalf("the closed file changed or does not read: %v", err)
	}
}

// Only a pointer to a body frame is written, since the writer sets its `t`
// through the pointer. Anything else is refused and leaves no line and no
// count, so the file still closes and reads back.
func TestOnlyABodyFrameIsWritten(t *testing.T) {
	w, err := NewWriter(header())
	if err != nil {
		t.Fatal(err)
	}
	h := header()
	for _, frame := range []any{Target{ID: "t1", Statement: "s", Status: TargetOpen}, &h, "a line"} {
		if err := w.Write(frame); err == nil || !strings.Contains(err.Error(), "is not a body frame") {
			t.Errorf("%T: %v", frame, err)
		}
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil || len(f.Commit.Counts) != 0 || bytes.Count(data, []byte("\n")) != 2 {
		t.Fatalf("read %v, counts %v, file %s", err, f, data)
	}
}

// A frame JSON cannot carry is refused and not counted, so the file still
// closes with counts that match its lines and reads back.
func TestAFrameJSONCannotCarryIsNotCounted(t *testing.T) {
	w, err := NewWriter(header())
	if err != nil {
		t.Fatal(err)
	}
	nan := math.NaN()
	if err := w.Write(&Cost{Scope: ScopeTarget, ID: "t1", Money: &nan}); err == nil || !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("a cost of NaN: %v", err)
	}
	if err := w.Write(&Target{ID: "t1", Statement: "s", Status: TargetOpen, Talks: []string{}, Cite: []Cite{}}); err != nil {
		t.Fatal(err)
	}
	data, _, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the file does not read back: %v", err)
	}
	if len(f.Costs) != 0 || len(f.Targets) != 1 {
		t.Fatalf("%d costs, %d targets", len(f.Costs), len(f.Targets))
	}
}

// A number JSON cannot carry, such as a cost that came out as NaN, fails the
// header when it is written. A writer that went on would make a file with
// no header.
func TestAHeaderJSONCannotCarryIsRefused(t *testing.T) {
	h := header()
	nan := math.NaN()
	h.ReportedCostUSD = &nan
	if w, err := NewWriter(h); w != nil || err == nil || !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("writer %v, error %v", w, err)
	}
}
