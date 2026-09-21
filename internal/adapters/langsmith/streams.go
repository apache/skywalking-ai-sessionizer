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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A nested run with its own model context belongs in a stream of its own.
//
// Measured on the subagent capture: a graph calls a tool, and the tool's
// implementation runs a second graph with its own message list. Those model
// calls continue nothing in the caller's list. Landing them in main makes a
// stream where one call's history does not follow from the call before it,
// which is exactly what a stream is defined to rule out, and the reader sees
// one agent doing work that two did.
//
// The boundary is the tool run. Anything below a tool run is work the tool
// started, so it lands in that tool's own stream; the tool run itself stays
// with its caller, because the call and its result are the caller's evidence.
// This is the same shape Claude Code has, where the Task call and its result
// sit in the main transcript and the child's own turns sit in a sidecar.
//
// A tool run with nothing below it opens no stream. It is an ordinary step.

// shapeFile is what a session remembers about its traces between requests.
//
// Only tool runs are kept, because only a tool run can open a stream, and a
// run arriving later names its ancestors by id alone. It is derived and
// disposable in the same way the index is: delete it and the placement of
// records already landed does not change, because their stream is the
// directory they are in.
const shapeFile = "langsmith.shape.json"

// shapeVersion is bumped when the file's meaning changes. A file from an
// older version is discarded rather than migrated.
const shapeVersion = 2

// toolRun is what a session remembers about one tool run.
type toolRun struct {
	Name string `json:"name,omitempty"`
	// Call is the call this tool answered, which is what a stream is
	// joined by. It is the call's id, never the tool's name: a spawn is
	// resolved by looking the call up by id.
	Call string `json:"call,omitempty"`
	// Program and PlainCall record what has run directly under this tool:
	// a chain of any kind, or only model calls. A program in there makes
	// it a child agent; only calls make it a plain call the agent made with
	// a prompt of its own, which is kept apart from the caller's stream for
	// the continuity check and for nothing else. Anything that is not a
	// model call counts as a program, so that when in doubt the answer is
	// the one that has always been given.
	Program   bool `json:"program,omitempty"`
	PlainCall bool `json:"plain_call,omitempty"`
	// Stream is the stream this tool run itself belongs to, which is its
	// caller's. A link landed for it has to go there: a tool nested inside
	// another agent's tool belongs to that agent, and sending its link to
	// the parent lineage put it under the wrong one.
	Stream string `json:"stream,omitempty"`
	// Joined records that the link from this tool to the stream it started
	// has been landed, so it is landed once and not on every pass. JoinedAs
	// says what the link said: a link that said auxiliary is landed again,
	// without it, once a program turns up under the tool.
	Joined   bool   `json:"joined,omitempty"`
	JoinedAs string `json:"joined_as,omitempty"`
}

type shape struct {
	Version int `json:"version"`
	// Tools maps a tool run's id to what is known about it.
	Tools map[string]toolRun `json:"tools"`
	// Named records that the conversation has been given its name, so it is
	// landed once and does not change with every turn. NamedBy says where it
	// came from: a name taken from a run, because nothing had been asked
	// yet, gives way to the question once it arrives.
	Named   bool   `json:"named,omitempty"`
	NamedBy string `json:"named_by,omitempty"`
	// Opened holds the tool runs that turned out to have work inside them.
	// A tool the graph ran and nothing else is an ordinary step and is not
	// here.
	Opened map[string]bool `json:"opened"`
}

func loadShape(path string) *shape {
	s := &shape{Version: shapeVersion, Tools: map[string]toolRun{}, Opened: map[string]bool{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var loaded shape
	if json.Unmarshal(data, &loaded) != nil || loaded.Version != shapeVersion {
		return s
	}
	if loaded.Tools == nil {
		loaded.Tools = map[string]toolRun{}
	}
	if loaded.Opened == nil {
		loaded.Opened = map[string]bool{}
	}
	return &loaded
}

func (s *shape) save(path string) error {
	return storage.WriteAtomic(path, 0o644, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	})
}

// observe remembers a run that could later turn out to have started work.
//
// The call it answered is kept with it. The link from a tool to the stream
// it started can only be landed once both are known, and those can arrive in
// either order, so whichever comes second has to find the first.
func (s *shape) observe(run Run, call string) {
	if run.Type != "tool" || run.ID == "" {
		return
	}
	known := s.Tools[run.ID]
	if run.Name != "" {
		known.Name = run.Name
	}
	if call != "" {
		known.Call = call
	}
	// Worked out before this run is remembered as a tool, so a tool never
	// finds itself among its own ancestors.
	if known.Stream == "" {
		known.Stream = s.streamOf(run)
	}
	s.Tools[run.ID] = known
}

// streamOf says which stream a run's records belong to.
//
// The answer is the deepest tool run among the run's ancestors. The ancestors
// come from the run's own dotted order, which every arrival carries, so a
// patch places itself without its post having to be remembered.
func (s *shape) streamOf(run Run) string {
	for _, id := range ancestors(run.Dotted, run.ID) {
		if known, ok := s.Tools[id]; ok {
			return StreamName(id, known.Name)
		}
	}
	return MainStream
}

// markOpened records that this run ran inside a tool, which is what turns
// that tool from a step into the start of a stream.
//
// It runs after every tool run of the request has been observed, because a
// request may carry a child before the tool it ran inside.
func (s *shape) markOpened(run Run) {
	for _, id := range ancestors(run.Dotted, run.ID) {
		if _, isTool := s.Tools[id]; isTool {
			s.Opened[id] = true
		}
	}
	// What ran directly under a tool is what says whether the tool ran a
	// program or made a call. A model call two levels down is inside a
	// program already, so only the direct child is read.
	if known, isTool := s.Tools[run.ParentID]; isTool && run.ParentID != "" {
		if run.Type == "llm" || run.Type == "chat_model" {
			known.PlainCall = true
		} else {
			known.Program = true
		}
		s.Tools[run.ParentID] = known
	}
}

// auxiliary reports that this tool opened a stream for a plain model call
// of its own, and not for a child agent.
func (s *shape) auxiliary(id string) bool {
	known, ok := s.Tools[id]
	return ok && known.PlainCall && !known.Program
}

// promptOf says which prompt a run belongs to: the tool it ran inside when
// it is nested, and its trace otherwise. Every stream of one trace would
// otherwise share the trace as its prompt, and a nested agent's records
// would name the caller's prompt as theirs.
func (s *shape) promptOf(run Run) string {
	for _, id := range ancestors(run.Dotted, run.ID) {
		if _, ok := s.Tools[id]; ok {
			return id
		}
	}
	return run.TraceID
}

// opens reports whether this run is a tool that started a stream.
func (s *shape) opens(run Run) bool {
	return run.Type == "tool" && s.Opened[run.ID]
}

// StreamName is what a child stream is called.
//
// The name has to come from stable evidence, never from position, so that the
// same child keeps the same name when an earlier request is collected again.
// The run id supplies that. Its first half is a timestamp shared by every run
// in the trace, so the tail is what distinguishes them.
func StreamName(runID, toolName string) string {
	slug := slugOf([]string{toolName})
	if slug == "" {
		slug = "tool"
	}
	return slug + "-" + tail(runID)
}

// tail is the last twelve hexadecimal characters of a run id.
func tail(runID string) string {
	plain := strings.ReplaceAll(runID, "-", "")
	if len(plain) <= 12 {
		return plain
	}
	return plain[len(plain)-12:]
}

// ancestors reads a run's ancestor ids out of its dotted order, deepest first
// and not including the run itself.
//
// A dotted order is segments joined by '.', each a start time followed by the
// run's id. Only the id is wanted here. A segment that does not end in one is
// skipped rather than guessed at.
func ancestors(dotted, self string) []string {
	if dotted == "" {
		return nil
	}
	segments := strings.Split(dotted, ".")
	var out []string
	for i := len(segments) - 1; i >= 0; i-- {
		id := idOfSegment(segments[i])
		if id == "" || id == self {
			continue
		}
		out = append(out, id)
	}
	return out
}

// uuidLen is the length of a run id as a dotted order writes it.
const uuidLen = 36

func idOfSegment(segment string) string {
	if len(segment) < uuidLen {
		return ""
	}
	id := segment[len(segment)-uuidLen:]
	for i, r := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return ""
			}
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return ""
		}
	}
	return id
}

// joined records that the link from this tool to its stream has been landed,
// and what it said.
func (s *shape) joined(id string, auxiliary bool) {
	known, ok := s.Tools[id]
	if !ok {
		return
	}
	known.Joined = true
	known.JoinedAs = "child"
	if auxiliary {
		known.JoinedAs = "auxiliary"
	}
	s.Tools[id] = known
}

// revised reports that a tool was joined as a plain call and has since had a
// program run under it, so what was said has to be said again.
func (s *shape) revised(id string) bool {
	known, ok := s.Tools[id]
	return ok && known.Joined && known.JoinedAs == "auxiliary" && !s.auxiliary(id)
}

// lateLink is a tool whose stream was discovered after its own records had
// already landed.
type lateLink struct {
	tool   string // the run
	call   string // the call it answered
	child  string // the stream that started inside it
	stream string // the stream the tool itself belongs to
	// auxiliary says the stream is a plain call the agent made, not a
	// child agent.
	auxiliary bool
}

// unjoined finds the tools that opened a stream and have not said so.
//
// A tool in this request is skipped: its own records carry the link already.
// Only a tool whose records were written earlier needs one landed for it.
func (s *shape) unjoined(here map[string]bool) []lateLink {
	var out []lateLink
	for id, known := range s.Tools {
		if !s.Opened[id] || here[id] || known.Call == "" {
			continue
		}
		if known.Joined && !s.revised(id) {
			continue
		}
		stream := known.Stream
		if stream == "" {
			stream = MainStream
		}
		out = append(out, lateLink{tool: id, call: known.Call,
			child: StreamName(id, known.Name), stream: stream, auxiliary: s.auxiliary(id)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].child < out[j].child })
	for _, link := range out {
		s.joined(link.tool, link.auxiliary)
	}
	return out
}

// record is the link, as a landed record.
//
// It carries the call and the stream and nothing else. From says the runtime
// produced it, because no arrival did: it is what the collector worked out
// from two of them.
//
// It carries no time either. The join did not happen at a moment; the call
// and the stream it names both did, and they carry their own.
func (l lateLink) record() sessiondata.Record {
	data, _ := json.Marshal(map[string]string{"tool_run": l.tool, "child_stream": l.child})
	var flags []string
	if l.auxiliary {
		flags = []string{"auxiliary"}
	}
	// The id says what the link said, so a link landed again to say
	// something else is a different record. The index keeps the first
	// record for an id, and with one id for both the correction was kept
	// on disk and never read: the stream stayed a plain call after the
	// program under it had arrived.
	kind := "child"
	if l.auxiliary {
		kind = "auxiliary"
	}
	return sessiondata.Record{
		ID: l.tool + ":started:" + kind, Tool: l.call, Child: l.child,
		From: sessiondata.FromRuntime, Flags: flags,
		Sha: shortSum(l.tool + l.child), Bytes: len(data),
		Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: data,
			State: "available", Bytes: len(data)}},
	}
}

// shortSum is a stable digest for a record the collector made itself.
func shortSum(of string) string {
	sum := sha256.Sum256([]byte(of))
	return hex.EncodeToString(sum[:])[:12]
}

// A conversation needs a name a person recognises.
//
// Nothing on this wire carries one. The client names runs - "LangGraph",
// "agent", "should_continue" - and those name the program rather than the
// conversation, so a list of them all says the same thing. What a reader
// recognises is what was asked.
//
// So the name is the first question of the conversation, and when it began
// without words - a traced function called with arguments rather than a
// message - the name of the run that started it. Both are evidence: they are
// landed from the arrival that carried them, and neither is invented.
//
// It is landed once. The fold takes the last name it finds, so naming every
// turn would make a conversation's name change under a reader each time it
// answered.

// titleLimit keeps a name to about a line. A question can be a page long,
// and the whole of it is in the conversation either way.
const titleLimit = 80

// titleRecord is the conversation's name, as a landed record.
//
// It carries a name and no identity of its own, which is what marks a record
// as naming the conversation rather than naming a step inside it.
// It carries no time. A record's time is evidence of when something
// happened, and a name did not happen: taking the collector's clock for it
// moved the conversation's last moment to whenever it was collected.
func titleRecord(name string) sessiondata.Record {
	return sessiondata.Record{
		Label: name,
		From:  sessiondata.FromRuntime,
		Sha:   shortSum("title:" + name),
		Bytes: len(name),
	}
}

// titleOf reads a conversation's name out of the first records to land in
// it, and says whether it is what was asked or only what ran.
func titleOf(items []placed) (name string, asked bool) {
	for _, item := range items {
		for _, record := range item.records {
			if record.Trigger != model.TriggerExternal {
				continue
			}
			for _, part := range record.Parts {
				if part.Kind == sessiondata.PartText && part.Text != "" {
					return trimTitle(part.Text), true
				}
			}
		}
	}
	// Nothing was asked in words. The run that began the conversation names
	// it instead, which for a traced function is the function.
	for _, item := range items {
		if item.run.ParentID == "" && item.run.Name != "" {
			return trimTitle(item.run.Name), false
		}
	}
	return "", false
}

// trimTitle takes the first line, and no more of it than a name should be.
func trimTitle(text string) string {
	text = strings.TrimSpace(text)
	if line, _, found := strings.Cut(text, "\n"); found {
		text = strings.TrimSpace(line)
	}
	runes := []rune(text)
	if len(runes) <= titleLimit {
		return text
	}
	// On a space where there is one near the end, so a name does not stop
	// in the middle of a word.
	cut := string(runes[:titleLimit])
	if at := strings.LastIndexByte(cut, ' '); at > titleLimit/2 {
		cut = cut[:at]
	}
	return strings.TrimSpace(cut) + "..."
}
