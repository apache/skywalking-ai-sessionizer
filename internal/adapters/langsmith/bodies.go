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
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// What each model call was sent, and what came back, landed beside the
// conversation.
//
// A call's record keeps what the model said. What it was told - the whole
// message list, sent again on every call - is the evidence the continuity
// check between calls runs on, the child's own opening prompt, and the one
// thing that tells a tool that summarised some text from one that summarised
// the conversation and started again from the summary. Without it the
// conversation is thin in exactly those places.
//
// It is landed as a provider body, cut against what the session already
// holds by pkg/providerbody, the same way Claude Code's bodies are. Measured
// on the captured corpus, with every call's inputs and outputs: over twenty
// turns 217 KB on the wire lands as 86 KB, because each request shares its
// front with the one before; on a conversation of a few short calls it lands
// as more than it was, because the manifest that says how to rebuild a body
// costs more than a small body saves. Both are stated on the adapter page.
//
// On this wire a body is the framework's view of what it sent - the run's
// inputs.messages - and not the bytes the provider received. The manifest
// says which runtime it came from, and a reader must not take it for the
// wire.
//
// A request is landed from the first arrival of a run that carries inputs,
// which never change; a response only from the arrival that carries the
// run's end, for the same reason the call's content is. A repeat is a
// repeat.
//
// Both are joined by their call, as internal/assemble/provider.go joins a
// body that names one. On this wire a request knows its call - it is the
// run's own id - where a request Claude Code writes names only the request
// before it and its prompt, and is joined by those. So nothing about the
// order of a stream's calls is kept or derived here. It was, twice: a copy
// kept in the collector's state, then an order read from the session's
// index, and each disagreed with the assembler in some schedule - a session
// landed before the copy existed, a call whose first fragment landed in main
// before its ancestry arrived, a request held for a later pass and then
// replayed - and each time a request joined another call. The run id is
// evidence; an order is an inference, and the assembler's is the only one.

// body is one body of one arrival, with what it joins by.
type body struct {
	run   Run
	role  string
	model string
	bytes []byte
	// prompt is what the run's records name as theirs. The manifest states
	// it; nothing joins by it.
	prompt string
}

// bodiesOf reads the bodies one request's arrivals carry, in arrival order.
func bodiesOf(items []placed) []body {
	var out []body
	for _, item := range items {
		if item.run.Type != "llm" && item.run.Type != "chat_model" {
			continue
		}
		var prompt string
		if len(item.records) > 0 {
			prompt = item.records[0].Run
		}
		if in := item.inputs; len(in) > 0 && string(in) != "null" {
			out = append(out, body{run: item.run, role: providerbody.RoleRequest, model: item.model, bytes: in, prompt: prompt})
		}
		if out2 := item.outputs; item.run.Finished() && len(out2) > 0 && string(out2) != "null" {
			out = append(out, body{run: item.run, role: providerbody.RoleResponse, model: item.model, bytes: out2, prompt: prompt})
		}
	}
	return out
}

// loadHeld rebuilds what the session already holds, from its landed bodies.
//
// A file that does not read whole holds nothing a new body may refer to; the
// new body then lands whole where nothing earlier is left to share, and asz
// verify reports the damage itself.
func (c *Collector) loadHeld(session string) (*providerbody.Session, error) {
	held := providerbody.NewSession()
	files, err := storage.LandedFiles(c.Zone, session)
	if err != nil {
		return nil, err
	}
	for _, lf := range files {
		if lf.Stream != "" || lf.RunID != "" {
			continue
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			continue
		}
		reader, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		var recs []*sessiondata.Record
		whole := true
		for {
			rec, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				whole = false
				break
			}
			recs = append(recs, rec)
		}
		f.Close()
		if !whole {
			continue
		}
		for _, rec := range recs {
			_ = held.Add(rec)
		}
	}
	return held, nil
}

// landed says what landing one request's bodies did.
type landedBodies struct {
	files, records, repeats, conflicts int
}

// landBodies writes one session's bodies out of one request, in files of the
// session's own sequence, under the session lock the caller holds.
func (c *Collector) landBodies(session string, state *storage.SessionState,
	bodies []body, stamp string, request Waiting) (landedBodies, error) {
	var out landedBodies
	if len(bodies) == 0 {
		return out, nil
	}
	held, err := c.loadHeld(session)
	if err != nil {
		return out, err
	}
	src := filepath.Base(request.Path)
	var batch []*sessiondata.Record
	var size int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		seq := state.Take()
		header := &sessiondata.Header{
			Seq: seq, At: c.Now().UTC().Format(time.RFC3339Nano),
			Kind: sessiondata.KindProviderBody, Adapter: Name + "/" + Version,
			Dialect: Dialect, Src: ".", Session: session,
		}
		dir := c.Zone.ProviderDir(session)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, storage.LandedName(string(sessiondata.KindProviderBody), stamp, seq))
		err := storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
			writer, err := sessiondata.NewWriter(w, header)
			if err != nil {
				return err
			}
			for _, rec := range batch {
				if err := writer.Write(rec); err != nil {
					return err
				}
			}
			return writer.Close()
		})
		if err != nil {
			return err
		}
		out.files++
		out.records += len(batch)
		batch, size = nil, 0
		return nil
	}
	for _, b := range bodies {
		keys := providerbody.Keys{Session: session, Model: b.model}
		id := b.run.ID + ":" + b.role
		switch b.role {
		case providerbody.RoleRequest:
			keys.Run, keys.Call = b.prompt, b.run.ID
		case providerbody.RoleResponse:
			keys.Call, keys.Request = b.run.ID, b.run.ID
		}
		rec, err := held.Encode(providerbody.Body{ID: id, Role: b.role, Src: src, Keys: keys, Bytes: b.bytes})
		switch {
		case errors.Is(err, providerbody.ErrRepeat):
			out.repeats++
			continue
		case err != nil:
			// The same run's body, delivered again with other bytes: an
			// update that changed what a finished run had said. The first
			// is kept, as the index keeps the first record for an id, and
			// this one is counted rather than landed under a second name
			// that would make the call's response ambiguous.
			out.conflicts++
			continue
		}
		n := providerbody.RecordBytes(rec)
		if len(batch) > 0 && providerbody.FileOverhead+size+n > c.MaxDeltaBytes {
			if err := flush(); err != nil {
				return out, err
			}
		}
		batch = append(batch, rec)
		size += n
	}
	return out, flush()
}
