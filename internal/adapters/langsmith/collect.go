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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// MainStream is where a thread's own runs land. A nested run with its own
// model context belongs in a stream of its own, which needs the whole trace to
// decide and is not decided here.
const MainStream = "main"

// Collector turns what the receiver accepted into landed files.
//
// It is deliberately a separate pass from the receiver. The receiver's job
// ends when a request is durable; this one takes as long as a session's lock
// makes it take, and a client waiting on a connection should never be paying
// for that.
type Collector struct {
	Zone      *storage.Zone
	Ownership Ownership
	// MaxDeltaBytes is the largest landed file a conversion writes. One
	// request can carry more: a single turn with a 20 KB command and a 32 KB
	// result measured 3 MB. A record larger than the budget lands alone,
	// because a file is cut by narrowing what goes in it and never by
	// splitting a record.
	MaxDeltaBytes int64
	// ProviderBodies lands what each model call was sent and what came
	// back, beside the conversation, cut against what the session already
	// holds. See bodies.go for what it costs and what it is worth.
	ProviderBodies bool
	Now            func() time.Time
}

// DefaultMaxDeltaBytes is the budget a collector uses when none is set.
const DefaultMaxDeltaBytes = 2 << 20

// Landed says what one pass did.
type Landed struct {
	Requests int
	Files    int
	Records  int
	Sessions []string
	// Unassigned counts the runs that supplied no identity, which land under
	// their own trace rather than being guessed into a conversation.
	Unassigned int
	// Bodies counts the provider bodies landed: what each call was sent and
	// what came back, beside the conversation. BodyConflicts counts the
	// bodies not landed because the same run's body had already landed
	// with other bytes: the first is kept, and this says how often a later
	// delivery disagreed with it.
	Bodies        int
	BodyConflicts int
	// Unreadable counts the requests moved aside because nothing could be
	// read out of them.
	Unreadable int
	// Waiting counts the requests held because the runs their ancestry
	// names have not arrived yet.
	Waiting int
	// Placed counts the requests landed after waiting as long as they may,
	// with whatever ancestry did arrive.
	Placed int
}

// Collect converts every waiting request, oldest first.
//
// One request that cannot be read does not stop the rest. It is moved aside
// with the reason, and collection carries on: a body that will never parse
// used to sit at the head of the inbox and keep everything behind it from
// ever being collected.
func (c *Collector) Collect() (Landed, error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	if len(c.Ownership.Keys) == 0 {
		c.Ownership = DefaultOwnership()
	}
	if c.MaxDeltaBytes <= 0 {
		c.MaxDeltaBytes = DefaultMaxDeltaBytes
	}
	inbox := NewInbox(c.Zone)
	waiting, err := inbox.List()
	if err != nil {
		return Landed{}, err
	}
	// Under a lock, because two collectors on one root would otherwise each
	// read this, change it and write it back, and one would lose what the
	// other learned.
	inboxDir := filepath.Join(c.Zone.Root(), InboxDir)
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		return Landed{}, err
	}
	pendingLock, err := storage.LockSession(inboxDir)
	if err != nil {
		return Landed{}, err
	}
	defer func() { _ = pendingLock.Unlock() }()
	pendingPath := filepath.Join(inboxDir, pendingFile)
	open := loadPending(pendingPath)
	var out Landed
	touched := map[string]bool{}
	// What was done before a failure is reported, not discarded. A later
	// request failing does not unland the files an earlier one wrote, and a
	// caller that is told nothing landed will not parse them.
	finish := func(err error) (Landed, error) {
		for s := range touched {
			out.Sessions = append(out.Sessions, s)
		}
		sort.Strings(out.Sessions)
		return out, err
	}
	// A request whose ancestry has not arrived is set aside and tried again
	// at the end of this pass, because the batch that overtook it is very
	// often already waiting further down the same inbox. Only what is still
	// unplaceable after that waits for another pass.
	var again []Waiting
	deferring := true
	for len(waiting) > 0 {
		for _, request := range waiting {
			if err := inbox.Read(&request); err != nil {
				return finish(err)
			}
			name := filepath.Base(request.Path)
			landed, err := c.convert(request, open)
			request.Body = nil
			if err != nil {
				var wait *unplaceable
				if errors.As(err, &wait) {
					// The runs this one ran inside have not arrived. The
					// client sends with several threads at once, so a batch
					// can overtake the one carrying its parents, and placing
					// it now would give one agent's work to another.
					if deferring {
						again = append(again, request)
						continue
					}
					// It has been tried again and the ancestry is still not
					// here. It waits for another pass, and is landed anyway
					// once it has waited as long as it may.
					if open.held(name) {
						out.Waiting++
						if err := open.save(pendingPath, c.Now()); err != nil {
							return finish(err)
						}
						continue
					}
					if err := inbox.Read(&request); err != nil {
						return finish(err)
					}
					landed, err = c.convertAnyway(request, open)
					request.Body = nil
					if err == nil {
						out.Placed++
					}
				}
			}
			if err != nil {
				var bad *unreadable
				if !errors.As(err, &bad) {
					// Something about this root, not about this request: a
					// full disk, a lock that would not open. Leaving the
					// request is right, because the next pass may succeed.
					return finish(fmt.Errorf("langsmith: %s: %w", name, err))
				}
				if err := inbox.Unreadable(request.Path, err.Error()); err != nil {
					return finish(err)
				}
				out.Unreadable++
				continue
			}
			// What this request taught is written down BEFORE the request
			// is removed. Either order loses something if the two are not
			// separated: saving afterwards loses a run's start to a crash in
			// between, and the bare update that follows it lands under a
			// name saying its identity was not supplied; saving beforehand
			// would lose a completion to a replay, if a completion were ever
			// forgotten here. Nothing is: a run is forgotten only by age, so
			// what is written is only ever more than it was.
			open.arrived(name)
			if err := open.save(pendingPath, c.Now()); err != nil {
				return finish(err)
			}
			// Only now: every session this request touched has its file, so
			// a crash before here converts the whole request again and the
			// index reads the repeats as duplicates.
			if err := inbox.Done(request.Path); err != nil {
				return finish(err)
			}
			out.Requests++
			out.Files += landed.Files
			out.Records += landed.Records
			out.Unassigned += landed.Unassigned
			out.Bodies += landed.Bodies
			out.BodyConflicts += landed.BodyConflicts
			for _, s := range landed.Sessions {
				touched[s] = true
			}
		}
		if !deferring {
			break
		}
		// Round two, over what was set aside, with everything the first
		// round learned. Nothing is set aside twice.
		waiting, again, deferring = again, nil, false
	}
	return finish(nil)
}

// unreadable marks a failure as being about this request's bytes rather than
// about the root. Only these are moved aside; anything else is left to be
// tried again.
type unreadable struct{ err error }

func (u *unreadable) Error() string { return u.err.Error() }
func (u *unreadable) Unwrap() error { return u.err }

// unplaceable marks a request whose runs name ancestors that have not
// arrived. It is not a failure: it is a request that is too early.
type unplaceable struct{ run string }

func (u *unplaceable) Error() string {
	return "run " + u.run + " names a run that has not arrived"
}

// placed is one arrival with the records it became, kept together because the
// stream those records land in is a property of the run, not of the record.
type placed struct {
	run     Run
	records []sessiondata.Record
	// call is the call a tool run answered, taken from the record rather
	// than worked out twice.
	call string
	// inputs and outputs are the arrival's own fields, as they came, for
	// the bodies a model call lands beside the conversation; model is what
	// the runtime said the call ran on.
	inputs, outputs json.RawMessage
	model           string
}

// grouped is one session's arrivals out of one request.
type grouped struct {
	session   string
	items     []placed
	files     int
	records   int
	bodies    int
	conflicts int
}

// convert reads one request into landed files, refusing to place a run whose
// ancestry has not arrived.
func (c *Collector) convert(request Waiting, open *pending) (Landed, error) {
	return c.convertWith(request, open, true)
}

// convertAnyway is convert for a request that has waited as long as it may.
// What is still unknown is placed in the parent lineage, which is what the
// adapter page says happens and why.
func (c *Collector) convertAnyway(request Waiting, open *pending) (Landed, error) {
	return c.convertWith(request, open, false)
}

func (c *Collector) convertWith(request Waiting, open *pending, wait bool) (Landed, error) {
	operations, err := operationsOfRequest(request)
	if err != nil {
		return Landed{}, &unreadable{err}
	}
	// An update carries only what changed, so what it leaves out is filled
	// in from what the run's own start said. This has to happen before
	// anything else reads the run: which conversation it belongs to, which
	// stream, and whether it is a model call all come from those fields.
	// One arrival each, not one per run. A batch can carry a run's start and
	// an update to it together, and keying this by run id kept only the last
	// of them - so both were then read as the update, and the start's kind,
	// trace and project were lost from the record that had carried them.
	runs := make([]Run, len(operations))
	sessions := make([]string, len(operations))
	methods := make([]string, len(operations))
	unassigned := 0
	for i, op := range operations {
		var envelope Run
		if err := json.Unmarshal(op.Envelope, &envelope); err != nil {
			return Landed{}, &unreadable{fmt.Errorf("run %s: %w", op.RunID, err)}
		}
		completed, known, found := open.complete(envelope)
		runs[i] = completed
		methods[i] = known.Method

		// Which conversation this belongs to, resolved here rather than
		// later, because a start and an update to it can be in one batch and
		// the update carries none of what deciding it needs. The start's
		// answer has to be available by the time the update is read.
		switch {
		case found && known.Session != "":
			sessions[i] = known.Session
		default:
			var missing bool
			sessions[i], missing = c.sessionOf(op, completed)
			if missing {
				unassigned++
			}
		}
		if completed.Type != "" {
			open.started(completed, methodOf(op), sessions[i], c.Now())
		}
		open.sawRun(completed, c.Now())
	}

	// Every run of this request is now remembered, so an ancestor still
	// missing is one that has not arrived at all.
	if wait {
		for i := range operations {
			if !open.placeable(runs[i]) {
				return Landed{}, &unplaceable{run: runs[i].ID}
			}
		}
	}

	byRun := make(map[string]Run, len(operations))
	for i, op := range operations {
		if _, seen := byRun[op.RunID]; !seen || runs[i].Type != "" {
			byRun[op.RunID] = runs[i]
		}
	}
	hints := ResolveWith(operations, byRun)
	// Whether a trace has a model call in it is a fact about the trace, not
	// about this request. A batch carrying a root's completion and a failed
	// tool looks complete on its own, and a model call that landed earlier
	// is still a model call.
	hints.SelfContained = map[string]bool{}
	for _, run := range runs {
		if run.TraceID != "" && open.wholeTrace(run.TraceID) {
			hints.SelfContained[run.TraceID] = true
		}
	}
	bySession := map[string]*grouped{}
	var order []string
	var out Landed
	out.Unassigned = unassigned
	for i, op := range operations {
		envelope := runs[i]
		session := sessions[i]
		// A run may be updated after it ends as well as before, and that
		// update carries none of this. So a run is remembered whether or not
		// it has finished, and is forgotten only by age. Forgetting it here
		// lost its start to any replay of this request.
		open.started(envelope, methodOf(op), session, c.Now())
		records, err := ConvertRun(op, envelope, methods[i], op.Op == "post", hints)
		if err != nil {
			return out, &unreadable{fmt.Errorf("run %s: %w", op.RunID, err)}
		}
		g := bySession[session]
		if g == nil {
			g = &grouped{session: session}
			bySession[session] = g
			order = append(order, session)
		}
		item := placed{run: envelope, records: records}
		if d, err := decodeOperation(op); err == nil {
			item.inputs, item.outputs = d.inputs, d.outputs
			item.model, _ = d.metadata["ls_model_name"].(string)
		}
		if envelope.Type == "tool" {
			for _, r := range records {
				if r.Tool != "" {
					item.call = r.Tool
					break
				}
			}
		}
		g.items = append(g.items, item)
	}
	// One time for the whole pass: it names every file the pass lands and is
	// the collected time in each file's header, as in every other adapter, so
	// a reader that has only the header, such as a server that stores files by
	// session and sequence, derives the file's name from it.
	now := c.Now().UTC()
	// Every session of the request is checked before any is landed. Checking
	// one at a time landed the first and then set the request aside on the
	// second, and the first was indexed but never reported for parsing.
	// Nothing lands from a request that cannot land whole.
	for _, session := range order {
		if err := c.check(bySession[session], open, request); err != nil {
			return out, err
		}
	}
	for _, session := range order {
		g := bySession[session]
		if len(g.items) == 0 {
			continue
		}
		if err := c.land(g, now, request, open); err != nil {
			return out, err
		}
		out.Files += g.files
		out.Records += g.records
		out.Bodies += g.bodies
		out.BodyConflicts += g.conflicts
		out.Sessions = append(out.Sessions, session)
	}
	return out, nil
}

// sessionOf decides where a run's evidence lands.
//
// A run with no supplied key is not given one: it lands under its trace, in a
// session whose name says the identity was not supplied, and nothing merges it
// with anything else.
func (c *Collector) sessionOf(op Operation, envelope Run) (string, bool) {
	// The client splits extra out of the envelope only when it is large
	// enough to be worth a part of its own, so both places have to be read.
	// Reading the envelope's extra as though it were already the metadata
	// was the bug this comment exists to keep fixed: it put the whole extra
	// object where the metadata belonged, no supplied key was ever found,
	// and every conversation landed as one whose identity was not supplied.
	var extra struct {
		Metadata map[string]any `json:"metadata"`
	}
	if raw, ok := op.Fields["extra"]; ok {
		_ = json.Unmarshal(raw, &extra)
	} else {
		var inline map[string]any
		unmarshalInto(op.Envelope, "extra", &inline)
		extra.Metadata, _ = inline["metadata"].(map[string]any)
	}
	owner, ok := c.Ownership.Owner(envelope.Session, extra.Metadata)
	if !ok {
		// Its own trace, or its own run when it has no trace either. A
		// client can create a run with neither, and naming them all after
		// the empty string put every unrelated run in one conversation.
		return UnassignedID(traceOrRun(envelope)), true
	}
	return StorageID(owner), false
}

// traceOrRun is what identifies a run that supplied no conversation: its
// trace, or itself when the client sent no trace either.
func traceOrRun(run Run) string {
	if run.TraceID != "" {
		return run.TraceID
	}
	return run.ID
}

// placement is where one session's records of one request go, decided
// without creating anything for the session.
type placement struct {
	shape     *shape
	shapePath string
	streams   []string
	byStream  map[string][]sessiondata.Record
	// placed holds each item's records as placed, in item order, for what is
	// read from them after placement, such as the prompt a body names.
	placed [][]sessiondata.Record
}

// place decides where every record of one session goes: no directory, no
// lock, no state. It is called twice for a request that lands - once for
// every session before anything is written, and again by land - and both
// times it starts from the shape on disk, so the first has no effect.
// Reading the shape without the session lock is safe because one lock, on
// the inbox, already serialises whole passes across collectors.
func (c *Collector) place(g *grouped, open *pending) *placement {
	dir := c.Zone.SessionDir(g.session)
	// Two passes, so that a request carrying a child before the tool it ran
	// inside places both the same way a request carrying them in tree order
	// would.
	shapePath := filepath.Join(dir, shapeFile)
	sh := loadShape(shapePath)
	// What the root already knows about these traces, so a run is placed
	// from the same knowledge that said it could be placed. A tool seen in
	// an earlier request of this pass has not landed yet, and reading only
	// what has landed put a child in the parent lineage although everything
	// it needed had arrived.
	for _, item := range g.items {
		for id, name := range open.toolsOf(item.run.TraceID) {
			if _, known := sh.Tools[id]; !known {
				sh.Tools[id] = toolRun{Name: name}
			}
		}
	}
	for _, item := range g.items {
		sh.observe(item.run, item.call)
	}
	for _, item := range g.items {
		sh.markOpened(item.run)
	}

	// A tool that started a stream says so on its own records. That is the
	// join assembly resolves: the call is the parent's evidence and the
	// stream is the child's, and this is the only record that sees both.
	byStream := map[string][]sessiondata.Record{}
	var streams []string
	here := map[string]bool{}
	add := func(stream string, records ...sessiondata.Record) {
		if _, seen := byStream[stream]; !seen {
			streams = append(streams, stream)
		}
		byStream[stream] = append(byStream[stream], records...)
	}
	// The conversation's name, from the first records to land in it. Once,
	// unless the first name was only a run's - a root's completion can land
	// before its start, and then nothing had been asked yet. The question
	// replaces that when it arrives, and nothing replaces a question.
	if name, asked := titleOf(g.items); name != "" && (!sh.Named || (asked && sh.NamedBy == "run")) {
		add(MainStream, titleRecord(name))
		sh.Named = true
		sh.NamedBy = "run"
		if asked {
			sh.NamedBy = "question"
		}
	}
	// Placement works on copies and leaves the request's records as they
	// arrived. It runs once to check that a request can land and again to
	// land it, and changing the records made the second run build on the
	// first: a link landed with its auxiliary flag twice.
	placed := make([][]sessiondata.Record, len(g.items))
	for i, item := range g.items {
		stream := sh.streamOf(item.run)
		prompt := ""
		if stream != MainStream {
			// A nested stream's records carry its own prompt - the tool it
			// ran inside - rather than the trace's, so every stream of one
			// trace does not share one prompt.
			prompt = sh.promptOf(item.run)
		}
		for _, r := range item.records {
			r.Flags = append([]string(nil), r.Flags...)
			if prompt != "" && r.Run != "" {
				r.Run = prompt
			}
			if isReset(r) {
				// A reset belongs to the stream whose model call was sent the
				// summary, so its ids name that stream. It lands once per
				// stream while the shape remembers it.
				r.ID = stream + "/" + r.ID
				if r.Parent != "" {
					r.Parent = stream + "/" + r.Parent
				}
				if sh.Resets[r.ID] {
					continue
				}
				sh.Resets[r.ID] = true
			}
			placed[i] = append(placed[i], r)
		}
	}
	for i, item := range g.items {
		here[item.run.ID] = true
		records := placed[i]
		if sh.opens(item.run) {
			child := StreamName(item.run.ID, item.run.Name)
			plain := sh.auxiliary(item.run.ID)
			for j := range records {
				records[j].Child = child
				if plain {
					records[j].Flags = append(records[j].Flags, "auxiliary")
				}
			}
			sh.joined(item.run.ID, plain)
		}
		add(sh.streamOf(item.run), records...)
	}
	// A tool whose own records landed before anything ran inside it says
	// nothing about the stream that turned out to start there: only the
	// records of this request are marked, and its were written in an
	// earlier one. Without this the child stream is real, nothing names
	// the call that started it, and assembly reports it as an orphan.
	//
	// The link is landed on its own, once, in the caller's stream. It is
	// evidence in the same sense the rest is: the call comes from the tool
	// run's own result and the stream from the runs that ran inside it.
	for _, link := range sh.unjoined(here) {
		add(link.stream, link.record())
	}
	sort.Strings(streams)
	return &placement{shape: sh, shapePath: shapePath, streams: streams, byStream: byStream, placed: placed}
}

// check says whether every record of one session can be written at all.
func (c *Collector) check(g *grouped, open *pending, request Waiting) error {
	p := c.place(g, open)
	for _, stream := range p.streams {
		if err := c.encodable(g.session, stream, p.byStream[stream], request); err != nil {
			return err
		}
	}
	return nil
}

// land writes one session's records, one file per stream it touched.
//
// The lock is what makes the sequence monotonic across every stream of a
// session, and the sequence is what lets assembly track its progress with one
// watermark. Two requests carrying the same session are therefore serialised
// here, however they arrived.
func (c *Collector) land(g *grouped, now time.Time, request Waiting, open *pending) error {
	dir := c.Zone.SessionDir(g.session)
	p := c.place(g, open)
	sh, shapePath, streams, byStream := p.shape, p.shapePath, p.streams, p.byStream

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := storage.LockSession(dir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	statePath := c.Zone.SessionStatePath(g.session)
	state, err := storage.LoadSessionState(statePath, g.session)
	if err != nil {
		return err
	}
	if err := state.RecoverNextSeq(dir); err != nil {
		return err
	}
	for _, stream := range streams {
		if err := c.landStream(g, state, stream, byStream[stream], now, request); err != nil {
			return err
		}
	}
	if c.ProviderBodies {
		landed, err := c.landBodies(g.session, state, bodiesOf(g.items, p.placed), now, request)
		if err != nil {
			return err
		}
		g.files += landed.files
		g.bodies += landed.records
		g.conflicts += landed.conflicts
	}
	if err := c.indexLanded(g.session, state, c.Now()); err != nil {
		return err
	}
	if err := sh.save(shapePath); err != nil {
		return err
	}
	return state.Save(statePath, c.Now())
}

// indexLanded extends the session's index to what has just landed.
//
// The pipeline parses from the index and never extends it: a parse rebuilds
// an index only when there is none. So a collector that lands and does not
// index leaves everything after the first parse invisible. The files are
// there, the chain never reaches them, and the conversation is frozen at its
// first turn. The Claude Code collector does this while landing; this one did
// not, and nothing noticed until a test parsed a session twice.
//
// It runs under the session lock, after the files are written and before the
// session's state is saved, so what the index claims to cover is never more
// than what is on disk. An index from another schema is discarded and built
// again, never migrated.
func (c *Collector) indexLanded(session string, state *storage.SessionState, now time.Time) error {
	indexDir := c.Zone.IndexDir(session)
	ixState, err := storage.LoadIndexState(c.Zone.IndexStatePath(session), session)
	if err != nil {
		return err
	}
	// Only an index that agrees with its saved state is extended; one that
	// does not is built again. The guard lives in index.LoadFor because it
	// has to be the same in every writer: a guard here alone was defeated
	// when the changes collector ran first after a crash and saved counts
	// that matched.
	ix, ok, err := index.LoadFor(indexDir, session, ixState)
	if err != nil {
		return err
	}
	if !ok {
		ix = index.New(session)
		ixState = storage.NewIndexState(session)
	}
	landedTo := state.NextSeq - 1
	if ixState.IndexedSeq >= landedTo {
		return nil
	}
	if _, err := index.Rebuild(c.Zone, session, ix, ixState.IndexedSeq); err != nil {
		return err
	}
	ixState.Schema = index.Schema
	ixState.IndexedSeq = landedTo
	ixState.Entries = len(ix.Entries)
	ixState.Blocks = len(ix.Blocks)
	ixState.Strings = ix.Strings.Len()
	if err := ix.Write(indexDir); err != nil {
		return err
	}
	return ixState.Save(c.Zone.IndexStatePath(session), now)
}

// encodable writes a stream's records to nowhere, and says whether one of
// them can never be written at all.
func (c *Collector) encodable(session, stream string, records []sessiondata.Record,
	request Waiting) error {
	if len(records) == 0 {
		return nil
	}
	// The sequence is not the real one; nothing is written under it.
	header := &sessiondata.Header{
		Seq: 1, At: c.Now().UTC().Format(time.RFC3339Nano),
		Kind: sessiondata.KindTranscript, Adapter: Name + "/" + Version,
		Dialect: Dialect, Src: filepath.Base(request.Path),
		Session: session, Stream: stream,
	}
	writer, err := sessiondata.NewWriter(io.Discard, header)
	if err != nil {
		return err
	}
	for i := range records {
		record := records[i]
		record.Ord, record.Off = uint64(i+1), 0
		if err := writer.Write(&record); err != nil {
			if errors.Is(err, sessiondata.ErrNotEncodable) {
				return &unreadable{err}
			}
			return err
		}
	}
	return writer.Close()
}

// landStream writes one stream's records out of one request.
func (c *Collector) landStream(g *grouped, state *storage.SessionState, stream string,
	records []sessiondata.Record, now time.Time, request Waiting) error {
	if len(records) == 0 {
		return nil
	}
	streamDir := c.Zone.StreamDir(g.session, stream)
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		return err
	}
	ord, off, err := nextPosition(streamDir)
	if err != nil {
		return err
	}
	for i := range records {
		records[i].Ord, records[i].Off = ord, off
		ord++
		off += uint64(records[i].Bytes) + 1
	}
	// One request can be larger than a landed file should be, so it is cut
	// across files here. The cut narrows what goes in a file and never splits
	// a record: a record over the budget lands alone in a file of its own,
	// which is the same rule every other landed file follows.
	for start := 0; start < len(records); {
		end, size := start, int64(0)
		for end < len(records) {
			next := size + int64(records[end].Bytes)
			if end > start && next > c.MaxDeltaBytes {
				break
			}
			size = next
			end++
		}
		seq := state.Take()
		header := &sessiondata.Header{
			Seq: seq, At: now.Format(time.RFC3339Nano),
			Kind: sessiondata.KindTranscript, Adapter: Name + "/" + Version,
			Dialect: Dialect, Src: filepath.Base(request.Path),
			Session: g.session, Stream: stream,
		}
		path := filepath.Join(streamDir, storage.LandedName("transcript", storage.Stamp(now), seq))
		batch := records[start:end]
		// A failure here is almost always the disk, and a disk is retried:
		// treating every write failure as a bad request took a valid batch
		// out of the retry it needed. The exception is a record that will
		// never encode, however often it is tried - a field nested so deeply
		// that it is valid alone and not inside a record. That one is set
		// aside, because retrying it stops everything behind it for ever.
		err = storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
			writer, err := sessiondata.NewWriter(w, header)
			if err != nil {
				return err
			}
			for i := range batch {
				if err := writer.Write(&batch[i]); err != nil {
					return err
				}
			}
			return writer.Close()
		})
		if errors.Is(err, sessiondata.ErrNotEncodable) {
			return &unreadable{err}
		}
		if err != nil {
			return err
		}
		g.files++
		g.records += len(batch)
		start = end
	}
	return nil
}

// nextPosition reads where the stream's records got to.
//
// Ordinals run 1, 2, 3 across every file of a stream with no gap, and a
// record's offset is the byte after the one before it ended. The filesystem is
// the authority: a counter kept beside the files could disagree with them after
// a crash, and this cannot.
func nextPosition(streamDir string) (uint64, uint64, error) {
	items, err := os.ReadDir(streamDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, 0, nil
		}
		return 0, 0, err
	}
	var newest string
	var highest uint64
	for _, it := range items {
		name := it.Name()
		if it.IsDir() || !strings.HasPrefix(name, "transcript-") || !strings.HasSuffix(name, ".sd") {
			continue
		}
		seq, ok := storage.LandedFileSeq(name)
		if !ok || seq < highest {
			continue
		}
		highest, newest = seq, name
	}
	if newest == "" {
		return 1, 0, nil
	}
	data, err := os.ReadFile(filepath.Join(streamDir, newest))
	if err != nil {
		return 0, 0, err
	}
	reader, err := sessiondata.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	var last *sessiondata.Record
	for {
		record, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		last = record
	}
	if last == nil {
		return 1, 0, nil
	}
	return last.Ord + 1, last.Off + uint64(last.Bytes) + 1, nil
}

// operationsOfRequest reads a waiting request back into its arrivals.
func operationsOfRequest(request Waiting) ([]Operation, error) {
	if strings.Contains(request.Header.ContentType, "multipart/") {
		parsed, err := ParseMultipart(bytes.NewReader(request.Body), request.Header.ContentType)
		if err != nil {
			return nil, err
		}
		return parsed.Operations, nil
	}
	return jsonOperations(request.Body, request.Header)
}

// jsonOperations reads the JSON shapes an older client sends: a batch of runs,
// or one run on its own.
func jsonOperations(body []byte, header Received) ([]Operation, error) {
	var batch struct {
		Post  []json.RawMessage `json:"post"`
		Patch []json.RawMessage `json:"patch"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, fmt.Errorf("the body is neither multipart nor a JSON batch: %w", err)
	}
	if len(batch.Post) == 0 && len(batch.Patch) == 0 {
		return singleRun(body, header)
	}
	var out []Operation
	add := func(op string, runs []json.RawMessage) error {
		for _, raw := range runs {
			var envelope Run
			if err := json.Unmarshal(raw, &envelope); err != nil {
				return err
			}
			out = append(out, Operation{Op: op, RunID: envelope.ID, Envelope: raw,
				Fields: map[string]json.RawMessage{}, Order: len(out) + 1, Bytes: len(raw)})
		}
		return nil
	}
	if err := add("post", batch.Post); err != nil {
		return nil, err
	}
	if err := add("patch", batch.Patch); err != nil {
		return nil, err
	}
	return out, nil
}

// singleRun reads the documented one-run endpoints: POST /runs starts a run
// and PATCH /runs/{id} ends it.
//
// The method is what separates them. Without it the same body means two
// different things, and reading it as a post either way would land a finished
// run as one that had only started.
func singleRun(body []byte, header Received) ([]Operation, error) {
	var envelope Run
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.ID == "" {
		return nil, errors.New("the body carries no run with an id")
	}
	op := "post"
	if strings.EqualFold(header.Method, http.MethodPatch) || strings.EqualFold(header.Method, http.MethodPut) {
		op = "patch"
	}
	return []Operation{{Op: op, RunID: envelope.ID, Envelope: json.RawMessage(body),
		Fields: map[string]json.RawMessage{}, Order: 1, Bytes: len(body)}}, nil
}

// methodOf reads how the runtime said this run was traced, from whichever
// place the client put its extra.
func methodOf(op Operation) string {
	var extra struct {
		Metadata struct {
			Method string `json:"ls_method"`
		} `json:"metadata"`
	}
	if raw, ok := op.Fields["extra"]; ok {
		_ = json.Unmarshal(raw, &extra)
	} else {
		var inline map[string]json.RawMessage
		if err := json.Unmarshal(op.Envelope, &inline); err == nil {
			if raw, ok := inline["extra"]; ok {
				_ = json.Unmarshal(raw, &extra)
			}
		}
	}
	return extra.Metadata.Method
}
