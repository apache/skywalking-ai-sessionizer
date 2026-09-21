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

package langsmith

import (
	"encoding/json"
	"io"
	"os"
	"sort"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// An update to a run carries only what changed.
//
// The documented way to end a run by hand is update_run(id, outputs=...),
// and the client sends exactly that: the id, the outputs, and null for the
// trace, the project, the type and the dotted order. Read on its own that is
// a run belonging to no conversation, of no kind, and it landed as one -
// filed under a name that says its identity was not supplied, treated as
// framework bookkeeping, and its output dropped. The call it was ending
// stayed unfinished for ever.
//
// So the runs that have been started and not yet ended are remembered, and
// an arrival that leaves those fields out is completed from what its own
// start said. Nothing is invented: every field comes from an earlier arrival
// about the same run.

// pendingFile holds the runs that were started and have not ended.
const pendingFile = "open.json"

// pendingVersion is bumped when the file's meaning changes. An older file is
// discarded rather than migrated, which costs the completion of whatever was
// in flight at the time and nothing else.
const pendingVersion = 1

// pendingLimit bounds the file. A run that never ends would otherwise be
// remembered for ever, and a client that crashes mid-trace leaves many. When
// there are more than this, the oldest go: they are the least likely to still
// be updated.
const pendingLimit = 50000

// pendingMaxAge is how long a started run is worth remembering. Longer than
// any turn, far shorter than for ever.
const pendingMaxAge = 24 * time.Hour

type pendingRun struct {
	Trace   string `json:"trace,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Type    string `json:"type,omitempty"`
	Name    string `json:"name,omitempty"`
	Dotted  string `json:"dotted,omitempty"`
	Project string `json:"project,omitempty"`
	Start   string `json:"start,omitempty"`
	// Method is what the runtime said about how this run was traced. An
	// update carries no metadata at all, and without this a decorated
	// function's completion is read as a graph's.
	Method string `json:"method,omitempty"`
	// Session is the conversation this run's evidence lands in, already
	// resolved. It is kept rather than derived again because the update
	// carries none of what deriving it needs.
	Session string `json:"session,omitempty"`
	// At is when this was remembered, for the eviction above.
	At string `json:"at,omitempty"`
}

// traceMemory is what a root remembers about one trace while it is in flight.
//
// Two questions need it, and neither can be answered from one request. Which
// agent a run belongs to is its ancestry, and an ancestor whose kind is not
// known makes the answer a guess. And whether a trace has a model call in it
// decides whether a tool that names no call may have one made up; a request
// holding no model call is not a trace holding none.
type traceMemory struct {
	// Runs are the runs of this trace that have been seen, and whether each
	// was a tool. Anything not here has not been seen, which is different
	// from having been seen and not being a tool.
	Runs map[string]bool `json:"runs,omitempty"`
	// ToolNames names the tool runs, so a session can place a run inside one
	// that has been seen but has not landed yet. Deciding a run may be
	// placed from one memory and then placing it from another put a child
	// in the parent lineage although everything it needed had arrived.
	ToolNames map[string]string `json:"tool_names,omitempty"`
	// Models records that a model call has been seen in this trace.
	Models bool `json:"models,omitempty"`
	// Root records that the trace's own root has been seen.
	Root bool   `json:"root,omitempty"`
	At   string `json:"at,omitempty"`
}

type pending struct {
	Version int                   `json:"version"`
	Runs    map[string]pendingRun `json:"runs"`
	// Traces is the memory above, by trace id.
	Traces map[string]*traceMemory `json:"traces,omitempty"`
	// Deferred counts the passes a request has been held for, so a request
	// whose ancestry never arrives is landed rather than held for ever.
	Deferred map[string]int `json:"deferred,omitempty"`
}

// deferLimit is how many passes a request may be held waiting for the runs
// its ancestry names. Three is well past a reordering between threads
// sending at the same moment, and short enough that a trace which will never
// complete does not hold its evidence out of the conversation.
const deferLimit = 3

func loadPending(path string) *pending {
	p := &pending{Version: pendingVersion, Runs: map[string]pendingRun{},
		Traces: map[string]*traceMemory{}, Deferred: map[string]int{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var loaded pending
	if json.Unmarshal(data, &loaded) != nil || loaded.Version != pendingVersion {
		return p
	}
	if loaded.Runs == nil {
		loaded.Runs = map[string]pendingRun{}
	}
	if loaded.Traces == nil {
		loaded.Traces = map[string]*traceMemory{}
	}
	if loaded.Deferred == nil {
		loaded.Deferred = map[string]int{}
	}
	return &loaded
}

func (p *pending) save(path string, now time.Time) error {
	p.evict(now)
	return storage.WriteAtomic(path, 0o644, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(p)
	})
}

// evict drops what is too old to still be updated, and then the oldest of
// whatever is left over the limit.
func (p *pending) evict(now time.Time) {
	cutoff := now.Add(-pendingMaxAge)
	for id, run := range p.Runs {
		at, err := time.Parse(time.RFC3339Nano, run.At)
		if err != nil || at.Before(cutoff) {
			delete(p.Runs, id)
		}
	}
	for trace, memory := range p.Traces {
		at, err := time.Parse(time.RFC3339Nano, memory.At)
		if err != nil || at.Before(cutoff) {
			delete(p.Traces, trace)
		}
	}
	if len(p.Traces) > pendingLimit {
		traces := make([]string, 0, len(p.Traces))
		for trace := range p.Traces {
			traces = append(traces, trace)
		}
		sort.Slice(traces, func(i, j int) bool { return p.Traces[traces[i]].At < p.Traces[traces[j]].At })
		for _, trace := range traces[:len(p.Traces)-pendingLimit] {
			delete(p.Traces, trace)
		}
	}
	if len(p.Runs) <= pendingLimit {
		return
	}
	ids := make([]string, 0, len(p.Runs))
	for id := range p.Runs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return p.Runs[ids[i]].At < p.Runs[ids[j]].At })
	for _, id := range ids[:len(p.Runs)-pendingLimit] {
		delete(p.Runs, id)
	}
}

// started remembers a run that has not ended yet.
//
// What is already known is kept when this arrival does not carry it, so a
// start seen earlier in the same batch is not erased by the update after it.
func (p *pending) started(run Run, method, session string, now time.Time) {
	known := p.Runs[run.ID]
	set := func(into *string, value string) {
		if value != "" {
			*into = value
		}
	}
	set(&known.Trace, run.TraceID)
	set(&known.Parent, run.ParentID)
	set(&known.Type, run.Type)
	set(&known.Name, run.Name)
	set(&known.Dotted, run.Dotted)
	set(&known.Project, run.Session)
	set(&known.Start, run.Start)
	set(&known.Method, method)
	set(&known.Session, session)
	known.At = now.UTC().Format(time.RFC3339Nano)
	p.Runs[run.ID] = known
}

// complete fills in what this arrival left out, from what the run's own
// start said. A field the arrival carries is never overwritten.
func (p *pending) complete(run Run) (Run, pendingRun, bool) {
	known, ok := p.Runs[run.ID]
	if !ok {
		return run, pendingRun{}, false
	}
	if run.TraceID == "" {
		run.TraceID = known.Trace
	}
	if run.ParentID == "" {
		run.ParentID = known.Parent
	}
	if run.Type == "" {
		run.Type = known.Type
	}
	if run.Name == "" {
		run.Name = known.Name
	}
	if run.Dotted == "" {
		run.Dotted = known.Dotted
	}
	if run.Session == "" {
		run.Session = known.Project
	}
	if run.Start == "" {
		run.Start = known.Start
	}
	return run, known, true
}

// sawRun remembers one run of a trace: that it was seen at all, and whether
// it was a tool.
func (p *pending) sawRun(run Run, now time.Time) {
	if run.TraceID == "" || run.ID == "" {
		return
	}
	memory := p.Traces[run.TraceID]
	if memory == nil {
		memory = &traceMemory{Runs: map[string]bool{}}
		p.Traces[run.TraceID] = memory
	}
	if memory.Runs == nil {
		memory.Runs = map[string]bool{}
	}
	if run.Type == "tool" {
		memory.Runs[run.ID] = true
		if memory.ToolNames == nil {
			memory.ToolNames = map[string]string{}
		}
		if _, named := memory.ToolNames[run.ID]; !named || run.Name != "" {
			memory.ToolNames[run.ID] = run.Name
		}
	} else if _, seen := memory.Runs[run.ID]; !seen {
		memory.Runs[run.ID] = false
	}
	if run.Type == "llm" || run.Type == "chat_model" {
		memory.Models = true
	}
	if run.ParentID == "" {
		memory.Root = true
	}
	memory.At = now.UTC().Format(time.RFC3339Nano)
}

// placeable reports whether every run this one ran inside has been seen.
//
// Until they have, which agent it belongs to is a guess: an ancestor that
// has not arrived may be the tool that starts a stream, and reading it as
// though it were not gives an independent agent's work to its caller.
func (p *pending) placeable(run Run) bool {
	memory := p.Traces[run.TraceID]
	for _, id := range ancestors(run.Dotted, run.ID) {
		if memory == nil {
			return false
		}
		if _, seen := memory.Runs[id]; !seen {
			return false
		}
	}
	return true
}

// wholeTrace reports that this trace's root has been seen and no model call
// ever has. Only then can a tool that names no call be said to have had no
// model ask for it.
func (p *pending) wholeTrace(trace string) bool {
	memory := p.Traces[trace]
	return memory != nil && memory.Root && !memory.Models
}

// held counts a request that is waiting for the runs its ancestry names, and
// says whether it may wait again.
func (p *pending) held(name string) bool {
	p.Deferred[name]++
	return p.Deferred[name] <= deferLimit
}

// arrived forgets a request that has been landed.
func (p *pending) arrived(name string) { delete(p.Deferred, name) }

// toolsOf names the tool runs of a trace that have been seen.
func (p *pending) toolsOf(trace string) map[string]string {
	memory := p.Traces[trace]
	if memory == nil {
		return nil
	}
	return memory.ToolNames
}
