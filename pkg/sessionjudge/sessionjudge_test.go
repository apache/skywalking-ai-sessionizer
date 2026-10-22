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
	if err := w.Write(&Input{Node: "msg/1", Talk: "talk/main/1", Actions: []Action{{Target: "t1", Action: ActionCreate}}}); err != nil {
		t.Fatal(err)
	}
	data, digest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return data, digest
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
	cases := map[string]struct{ body, err string }{
		// A header alone has no frame to count, so only the missing commit
		// says the file was cut short.
		"header only": {lines[0], "no commit"},
		// A second commit, closing the first, is content after the commit.
		"two commits": {rehash(string(data)), "content after the commit"},
		// The counts name exactly the frames there are, no more.
		"extra count": {lines[0] + lines[1] + lines[2] + strings.Replace(lines[3], `"counts":{`, `"counts":{"label":0,`, 1), "counts"},
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
