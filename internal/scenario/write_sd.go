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

package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/mock"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// writeSD lands a plan as Session Data, directly, in the model's vocabulary
// and under the mock dialect. It lands the way an adapter lands: one file
// per stream per build, a cursor per source so a build through a later
// checkpoint lands only what is new, sequences taken from the session's
// state, and every file write-once. asz verify passes on the result and the
// parser reads it like any other root.
func writeSD(p *Plan, root string, now time.Time) ([]string, error) {
	z := storage.NewZone(root)
	sessionDir := z.SessionDir(p.Session)
	if err := mkdirAll(sessionDir); err != nil {
		return nil, err
	}
	lock, err := storage.LockSession(sessionDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	statePath := z.SessionStatePath(p.Session)
	state, err := storage.LoadSessionState(statePath, p.Session)
	if err != nil {
		return nil, err
	}
	if err := state.RecoverNextSeq(sessionDir); err != nil {
		return nil, err
	}
	w := &sdWriter{p: p, z: z, state: state, now: now}

	// Streams first, main before its children, then the runs' files, so the
	// sequences a round refers to come out in landed order.
	byStream := map[string][]*Event{}
	for i := range p.Events {
		e := &p.Events[i]
		byStream[e.Stream] = append(byStream[e.Stream], e)
	}
	order := []string{"main"}
	for _, s := range p.Streams {
		order = append(order, s.ID)
	}
	for _, stream := range order {
		if err := w.stream(stream, byStream[stream]); err != nil {
			return nil, err
		}
	}
	for _, s := range p.Streams {
		if s.Batch != "" {
			continue
		}
		if err := w.meta(s); err != nil {
			return nil, err
		}
	}
	for _, r := range p.Runs {
		if err := w.run(r); err != nil {
			return nil, err
		}
	}
	if err := state.Save(statePath, now); err != nil {
		return nil, err
	}
	if err := w.reindex(); err != nil {
		return nil, err
	}
	sort.Strings(w.written)
	return w.written, nil
}

// reindex extends the session's index with what this build landed, as an
// adapter does while landing, so a parse after a later checkpoint sees the
// new files. The index is derived: it is rebuilt from the landed files, and
// a missing one is built from nothing.
func (w *sdWriter) reindex() error {
	dir := w.z.IndexDir(w.p.Session)
	ix, ok, err := index.Load(dir, w.p.Session)
	if err != nil {
		return err
	}
	st, err := storage.LoadIndexState(w.z.IndexStatePath(w.p.Session), w.p.Session)
	if err != nil {
		return err
	}
	if !ok {
		ix = index.New(w.p.Session)
		st = storage.NewIndexState(w.p.Session)
	}
	if _, err := index.Rebuild(w.z, w.p.Session, ix, st.IndexedSeq); err != nil {
		return err
	}
	if err := ix.Write(dir); err != nil {
		return err
	}
	st.Schema, st.Entries = index.Schema, len(ix.Entries)
	for i := range ix.Entries {
		if s := uint64(ix.Entries[i].Seq); s > st.IndexedSeq {
			st.IndexedSeq = s
		}
	}
	return st.Save(w.z.IndexStatePath(w.p.Session), w.now)
}

type sdWriter struct {
	p       *Plan
	z       *storage.Zone
	state   *storage.SessionState
	now     time.Time
	written []string
}

// source names a stream's or a run's file as the header's src: the mock has
// no source files, so the name says what the records are.
func (w *sdWriter) source(kind, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", w.p.Project, w.p.Session, kind, name)
}

// land writes one file of records the cursor has not seen, and moves the
// cursor. It returns false when nothing was new.
func (w *sdWriter) land(dir, prefix, cursorName string, hdr sessiondata.Header, recs []*sessiondata.Record) (bool, error) {
	if err := mkdirAll(dir); err != nil {
		return false, err
	}
	cursorPath := filepath.Join(dir, cursorName)
	cur, err := storage.LoadCursor(cursorPath, storage.CursorAppend, hdr.Src)
	if err != nil {
		return false, err
	}
	if uint64(len(recs)) <= cur.Ord {
		return false, nil
	}
	fresh := recs[cur.Ord:]
	seq := w.state.Take()
	hdr.H, hdr.Schema, hdr.Seq = 1, "", seq
	hdr.At = w.now.UTC().Format(time.RFC3339Nano)
	hdr.Adapter, hdr.Dialect, hdr.Session = mock.Name+"/"+mock.Version, mock.Dialect, w.p.Session
	path := filepath.Join(dir, storage.LandedName(prefix, storage.Stamp(w.now), seq))
	err = storage.WriteAtomic(path, storage.PermLanded, func(out io.Writer) error {
		rw, err := sessiondata.NewWriter(out, &hdr)
		if err != nil {
			return err
		}
		for _, r := range fresh {
			if err := rw.Write(r); err != nil {
				return err
			}
		}
		return rw.Close()
	})
	if err != nil {
		return false, err
	}
	cur.Ord, cur.LastSeq, cur.State = uint64(len(recs)), seq, storage.CursorActive
	if err := cur.Save(cursorPath, w.now); err != nil {
		return false, err
	}
	w.written = append(w.written, path)
	return true, nil
}

func (w *sdWriter) stream(stream string, events []*Event) error {
	if len(events) == 0 {
		return nil
	}
	recs := make([]*sessiondata.Record, 0, len(events))
	var off uint64
	for i, e := range events {
		r := w.record(e)
		finish(r, uint64(i+1), &off)
		recs = append(recs, r)
	}
	dir := w.z.StreamDir(w.p.Session, stream)
	hdr := sessiondata.Header{Kind: sessiondata.KindTranscript, Src: w.source("streams", stream), Stream: stream}
	_, err := w.land(dir, "transcript", "transcript.cursor", hdr, recs)
	return err
}

func (w *sdWriter) meta(s Stream) error {
	var off uint64
	r := &sessiondata.Record{Child: s.ID, Label: s.Label, From: sessiondata.FromRuntime,
		Parts: []sessiondata.Part{dataPart(map[string]any{"agentType": "general-purpose", "description": s.Label, "toolUseId": s.Tool, "spawnDepth": 1})}}
	finish(r, 1, &off)
	dir := w.z.StreamDir(w.p.Session, s.ID)
	hdr := sessiondata.Header{Kind: sessiondata.KindAgentMeta, Src: w.source("streams", s.ID+".meta"), Stream: s.ID}
	_, err := w.land(dir, "meta", "meta.cursor", hdr, []*sessiondata.Record{r})
	return err
}

func (w *sdWriter) run(r Run) error {
	dir := w.z.RunDir(w.p.Session, r.ID)
	var journal []*sessiondata.Record
	var off uint64
	for i, j := range r.Journal {
		rec := &sessiondata.Record{Child: j.Child, Batch: r.ID, From: sessiondata.FromRuntime}
		data := map[string]any{"type": j.Type, "key": "v2:" + j.Child, "agentId": j.Child}
		if j.Type == "result" {
			data["result"] = map[string]any{"done": true}
			rec.Flags = []string{"child_result"}
		}
		rec.Parts = []sessiondata.Part{dataPart(data)}
		finish(rec, uint64(i+1), &off)
		journal = append(journal, rec)
	}
	if _, err := w.land(dir, "journal", "journal.cursor", sessiondata.Header{Kind: sessiondata.KindJournal, Src: w.source("runs", r.ID+"/journal"), Batch: r.ID}, journal); err != nil {
		return err
	}
	off = 0
	m := &sessiondata.Record{Batch: r.ID, Label: r.Name, From: sessiondata.FromRuntime,
		Parts: []sessiondata.Part{dataPart(map[string]any{"runId": r.ID, "taskId": "task-" + r.ID, "workflowName": r.Name, "status": "completed", "agentCount": len(r.Children)})}}
	finish(m, 1, &off)
	if _, err := w.land(dir, "manifest", "manifest.cursor", sessiondata.Header{Kind: sessiondata.KindWorkflowManifest, Src: w.source("runs", r.ID+"/manifest"), Batch: r.ID}, []*sessiondata.Record{m}); err != nil {
		return err
	}
	off = 0
	src, _ := json.Marshal(r.Script)
	s := &sessiondata.Record{From: sessiondata.FromRuntime, Parts: []sessiondata.Part{{
		Kind: sessiondata.PartUnknown, Text: "the source is a program, not data", Data: src, State: "available", Bytes: len(r.Script)}}}
	finish(s, 1, &off)
	_, err := w.land(dir, "script", "script.cursor", sessiondata.Header{Kind: sessiondata.KindWorkflowScript, Src: w.source("runs", r.ID+"/script"), Batch: r.ID}, []*sessiondata.Record{s})
	return err
}

// record converts one event to the record an adapter would have written.
func (w *sdWriter) record(e *Event) *sessiondata.Record {
	r := &sessiondata.Record{ID: e.ID, Parent: e.Parent, Run: e.Run}
	if !e.At.IsZero() {
		r.Time = e.At.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	switch e.Kind {
	case EvTitle:
		r.From, r.Label = sessiondata.FromRuntime, e.Label
		r.Parts = []sessiondata.Part{dataPart(map[string]any{"type": "ai-title", "aiTitle": e.Label})}
	case EvInput:
		r.From = sessiondata.FromExternal
		if e.Stream == "main" {
			r.Trigger, r.Flags = model.TriggerExternal, []string{"external_input"}
		}
		r.Parts = []sessiondata.Part{textPart(e.Text)}
	case EvQueued:
		if e.Mode == "prompt" {
			r.From, r.Flags = sessiondata.FromExternal, []string{"external_input"}
			r.Parts = []sessiondata.Part{textPart(e.Text)}
		} else {
			r.From, r.Flags = sessiondata.FromRuntime, []string{"injected"}
			r.Parts = []sessiondata.Part{dataPart(map[string]any{"type": "queued_command", "commandMode": e.Mode, "content": e.Text})}
		}
	case EvInject:
		r.From, r.Flags = sessiondata.FromRuntime, []string{"injected"}
		r.Parts = []sessiondata.Part{dataPart(map[string]any{"type": e.Type, "content": e.Text})}
	case EvFragment:
		r.From, r.Call = sessiondata.FromAgent, e.Call
		u := e.Usage
		r.Usage = &sessiondata.Usage{Input: u.In, Output: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
		if e.Last {
			r.Flags = []string{"finished"}
		}
		switch e.Frag {
		case FragThinking:
			if e.Text == "" {
				r.Parts = []sessiondata.Part{{Kind: sessiondata.PartReasoning, State: "unavailable"}}
				r.Dropped = []sessiondata.Drop{{What: "reasoning signature", Bytes: 3, Why: "a provider verifies it; a reader cannot read it"}}
			} else {
				r.Parts = []sessiondata.Part{{Kind: sessiondata.PartReasoning, Text: e.Text, State: "available", Bytes: len(e.Text)}}
			}
		case FragText:
			r.Parts = []sessiondata.Part{textPart(e.Text)}
		case FragToolUse:
			input := e.Tool.Input
			if input == nil {
				input = map[string]any{}
			}
			data, _ := json.Marshal(input)
			r.Parts = []sessiondata.Part{{Kind: sessiondata.PartCall, Data: data, ID: e.Tool.ID, Name: e.Tool.Name, State: "available", Bytes: len(data)}}
		}
	case EvResult:
		r.From = sessiondata.FromExternal
		part := sessiondata.Part{Kind: sessiondata.PartResult, Text: e.Text, Of: e.Of, Failed: e.Failed, State: "available", Bytes: len(e.Text)}
		var data any = map[string]any{"stdout": e.Text, "stderr": ""}
		switch {
		case e.Ack != nil:
			r.Child, r.Flags = e.Ack.Child, []string{"launch_ack"}
			data = map[string]any{"agentId": e.Ack.Child, "isAsync": true, "status": "async_launched", "prompt": e.Ack.Prompt, "description": e.Ack.Label}
		case e.Fork != nil:
			r.Child = e.Fork.Child
			data = map[string]any{"agentId": e.Fork.Child, "status": "forked"}
		case e.Launch != nil:
			// A workflow launch is acknowledged the way an agent launch is.
			r.Batch, r.Label, r.Flags = e.Launch.Run, e.Launch.Name, []string{"launch_ack"}
			data = map[string]any{"runId": e.Launch.Run, "status": "async_launched", "transcriptDir": "subagents/workflows/" + e.Launch.Run, "workflowName": e.Launch.Name}
		case e.StringEnrichment:
			data = "a bare string, not an object"
		}
		if !e.Replayed {
			part.Data, _ = json.Marshal(data)
		}
		r.Parts = []sessiondata.Part{part}
	case EvNotice:
		r.From, r.Trigger, r.Tool, r.Child = sessiondata.FromExternal, model.TriggerNotification, e.NoticeTool, e.NoticeChild
		r.Parts = []sessiondata.Part{textPart(e.Text)}
	case EvSynthetic:
		r.From, r.Call, r.Flags = sessiondata.FromAgent, e.Call, []string{"synthetic"}
		r.Usage = &sessiondata.Usage{}
		r.Parts = []sessiondata.Part{textPart(e.Text)}
	case EvBoundary:
		r.From, r.Flags, r.Continues, r.Parent = sessiondata.FromRuntime, []string{"context_reset"}, e.Continues, ""
		r.Parts = []sessiondata.Part{dataPart(map[string]any{"type": "system", "subtype": "compact_boundary", "logicalParentUuid": e.Continues,
			"compactMetadata": map[string]any{"trigger": "auto", "preservedMessages": map[string]any{"allUuids": []any{e.Continues}}}})}
	case EvSummary:
		r.From, r.Flags = sessiondata.FromExternal, []string{"reset_summary"}
		r.Parts = []sessiondata.Part{textPart(e.Text)}
	case EvSystem:
		r.From = sessiondata.FromRuntime
		switch e.Type {
		case "turn_duration":
			r.Flags = []string{"turn_duration"}
		case "local_command":
			r.Flags = []string{"command"}
		default:
			r.Flags = []string{"notice"}
		}
		data := map[string]any{"type": "system", "subtype": e.Type}
		for k, v := range e.Fields {
			data[k] = v
		}
		r.Parts = []sessiondata.Part{dataPart(data)}
	}
	return r
}

func textPart(text string) sessiondata.Part {
	return sessiondata.Part{Kind: sessiondata.PartText, Text: text, State: "available", Bytes: len(text)}
}

func dataPart(v any) sessiondata.Part {
	data, _ := json.Marshal(v)
	return sessiondata.Part{Kind: sessiondata.PartData, Data: data, State: "available", Bytes: len(data)}
}

// finish sets a record's provenance: its ordinal in its stream, a running
// byte offset, and a digest of the record itself, since the mock has no
// source bytes to name.
func finish(r *sessiondata.Record, ord uint64, off *uint64) {
	r.Ord, r.Off = ord, *off
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	r.Sha, r.Bytes = hex.EncodeToString(sum[:6]), len(b)
	*off += uint64(len(b)) + 1
}
