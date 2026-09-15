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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// providerBodies joins the session's provider bodies to their calls. It
// returns how many bodies the session holds, and each call step's joined
// bodies.
//
// A response joins by its message id, which is the call's own. A request
// names no call, only the request before it and its prompt, so a request
// joins to the call of its stream whose previous call's response carries
// that request id, and whose prompt is the one it names, when exactly one
// request and exactly one call carry those two ids. A compaction request
// names no prompt, a retried request carries the same two ids twice, and the
// first calls of two streams under one prompt carry the same two ids; none of
// them is joined, and the entry says why. Nothing is joined by position or by
// time.
func (c *Conversation) providerBodies(landed []storage.LandedFile) (int, map[string][]sessionview.ProviderBody) {
	type body struct {
		id, step, join, role                    string
		ref                                     sessionflow.Ref
		promptID, prevRequest, requestID, msgID string
	}
	const (
		joinExact      = "exact"
		joinAmbiguous  = "ambiguous"
		joinUnresolved = "unresolved"
	)
	out := []body{}
	seen := map[string]bool{}
	for _, lf := range landed {
		if !strings.HasPrefix(filepath.Base(lf.Path), string(sessiondata.KindProviderBody)+"-") {
			continue
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			continue
		}
		rd, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for row := uint64(1); ; row++ {
			rec, err := rd.Next()
			if errors.Is(err, io.EOF) || err != nil {
				break
			}
			m, err := providerbody.ManifestOf(rec)
			if err != nil || seen[rec.ID] {
				// A body landed twice by an interrupted pass is one body.
				continue
			}
			seen[rec.ID] = true
			out = append(out, body{
				id: rec.ID, join: joinUnresolved, role: m.Role, ref: sessionflow.Ref{Seq: lf.Seq, Row: row},
				promptID: m.Run, prevRequest: m.PreviousRequest, requestID: m.Request, msgID: m.Call,
			})
		}
		f.Close()
	}
	byStep := map[string][]sessionview.ProviderBody{}
	if len(out) == 0 {
		return 0, byStep
	}

	// The calls, with their message id, their prompt and their stream, in
	// the order their records landed, which is line order in a stream.
	type call struct {
		id, stream, msg, prompt string
		at                      sessionflow.Ref
	}
	var calls []*call
	var refs []*sessionflow.Ref
	for _, n := range c.View.Nodes {
		if n.Kind != model.KindLLMCall || n.Ref == nil {
			continue
		}
		refs = append(refs, n.Ref)
		if p := c.View.Nodes[n.Parent]; p != nil && p.Kind == model.KindRun && p.Ref != nil {
			refs = append(refs, p.Ref)
		}
	}
	recs := c.records(refs)
	for _, n := range c.View.Nodes {
		if n.Kind != model.KindLLMCall || n.Ref == nil {
			continue
		}
		// A call whose record is gone stays in its stream with no message id,
		// so the call after it has no previous response to name and is left
		// unjoined rather than taken for the first of its stream.
		k := &call{id: n.ID, stream: n.Stream, at: *n.Ref}
		if rec := recs[[2]uint64{n.Ref.Seq, n.Ref.Row}]; rec != nil {
			// A synthetic record sits in the stream like a response, but no
			// provider was called: it has no request and no response, and
			// it is not the call before the next one.
			if slices.Contains(rec.Flags, "synthetic") {
				continue
			}
			k.msg = rec.Call
		}
		if p := c.View.Nodes[n.Parent]; p != nil && p.Kind == model.KindRun && p.Ref != nil {
			if run := recs[[2]uint64{p.Ref.Seq, p.Ref.Row}]; run != nil {
				k.prompt = run.Run
			}
		}
		calls = append(calls, k)
	}
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].stream != calls[j].stream {
			return calls[i].stream < calls[j].stream
		}
		if calls[i].at.Seq != calls[j].at.Seq {
			return calls[i].at.Seq < calls[j].at.Seq
		}
		if calls[i].at.Row != calls[j].at.Row {
			return calls[i].at.Row < calls[j].at.Row
		}
		// The nodes come from a map. Two calls on one record would otherwise
		// take their order from the map, and a request its call from that
		// order, so the same files could give two documents.
		return calls[i].id < calls[j].id
	})

	join := func(i int, step, quality string) {
		out[i].step, out[i].join = step, quality
	}
	// Responses, by message id.
	byMsg := map[string][]int{}
	for i, b := range out {
		if b.role == providerbody.RoleResponse && b.msgID != "" {
			byMsg[b.msgID] = append(byMsg[b.msgID], i)
		}
	}
	requestOf := map[string]string{} // call id -> the request id its response carries
	responseOf := map[string]int{}
	for _, k := range calls {
		if k.msg == "" {
			continue
		}
		switch hits := byMsg[k.msg]; len(hits) {
		case 0:
		case 1:
			join(hits[0], k.id, joinExact)
			requestOf[k.id], responseOf[k.id] = out[hits[0]].requestID, hits[0]
		default:
			for _, i := range hits {
				out[i].join = joinAmbiguous
			}
		}
	}
	// Requests, by the request before them and their prompt.
	type key struct{ prev, prompt string }
	byKey := map[key][]int{}
	for i, b := range out {
		if b.role == providerbody.RoleRequest && b.promptID != "" {
			k := key{b.prevRequest, b.promptID}
			byKey[k] = append(byKey[k], i)
		}
	}
	// The key each call's request would carry. A call whose previous call
	// has no response carrying its request id has none, and neither has a
	// call in a stream whose landed lines have a gap: a call may be missing
	// between two that look consecutive.
	gapped := map[string]bool{}
	for _, k := range calls {
		if _, done := gapped[k.stream]; !done {
			gapped[k.stream] = streamHasGap(landed, k.stream)
		}
	}
	callKey := map[string]key{}
	callsByKey := map[key]int{}
	for i, k := range calls {
		if gapped[k.stream] {
			continue
		}
		prev := ""
		if i > 0 && calls[i-1].stream == k.stream {
			var ok bool
			if prev, ok = requestOf[calls[i-1].id]; !ok || prev == "" {
				continue
			}
		}
		if k.prompt == "" {
			continue
		}
		callKey[k.id] = key{prev, k.prompt}
		callsByKey[key{prev, k.prompt}]++
	}
	requestFor := map[string]int{}
	for _, k := range calls {
		ck, ok := callKey[k.id]
		if !ok {
			continue
		}
		hits := byKey[ck]
		switch {
		case len(hits) == 0:
		case len(hits) == 1 && callsByKey[ck] == 1:
			// One request and one call carry the key: nothing else could be
			// this call's request, and this request no other call's.
			if out[hits[0]].join == joinUnresolved {
				join(hits[0], k.id, joinExact)
				requestFor[k.id] = hits[0]
			}
		default:
			for _, i := range hits {
				if out[i].join == joinUnresolved {
					out[i].join = joinAmbiguous
				}
			}
		}
	}
	for _, k := range calls {
		if i, ok := requestFor[k.id]; ok {
			byStep[k.id] = append(byStep[k.id], sessionview.ProviderBody{Role: out[i].role, Ref: out[i].ref})
		}
		if i, ok := responseOf[k.id]; ok {
			byStep[k.id] = append(byStep[k.id], sessionview.ProviderBody{Role: out[i].role, Ref: out[i].ref})
		}
	}
	return len(out), byStep
}

// streamHasGap reports whether a stream's landed transcript lines skip a
// line: the first is not line 1, or a line after a later one is missing.
// A line landed twice is a repeat, not a gap. Only each record's ord is read.
func streamHasGap(landed []storage.LandedFile, stream string) bool {
	var prev uint64
	for _, lf := range landed {
		if lf.Stream != stream || !strings.HasPrefix(filepath.Base(lf.Path), string(sessiondata.KindTranscript)+"-") {
			continue
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			return true
		}
		rd, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			return true
		}
		for {
			line, err := rd.NextRaw()
			if err != nil {
				break
			}
			ord, ok := leadingOrd(line)
			if !ok {
				f.Close()
				return true
			}
			if ord > prev+1 {
				f.Close()
				return true
			}
			if ord > prev {
				prev = ord
			}
		}
		f.Close()
	}
	return false
}

// leadingOrd reads a record line's ord, which the writer puts first.
func leadingOrd(line []byte) (uint64, bool) {
	const prefix = `{"ord":`
	if !bytes.HasPrefix(line, []byte(prefix)) {
		var r struct {
			Ord uint64 `json:"ord"`
		}
		if json.Unmarshal(line, &r) != nil {
			return 0, false
		}
		return r.Ord, true
	}
	var n uint64
	i := len(prefix)
	for ; i < len(line) && line[i] >= '0' && line[i] <= '9'; i++ {
		n = n*10 + uint64(line[i]-'0')
	}
	return n, i > len(prefix)
}

// annotateProvider writes each call step's joined bodies onto the step.
func annotateProvider(nodes []sessionview.Node, byStep map[string][]sessionview.ProviderBody) {
	for i := range nodes {
		if bodies := byStep[nodes[i].ID]; len(bodies) > 0 {
			nodes[i].ProviderBodies = bodies
		}
		annotateProvider(nodes[i].Children, byStep)
	}
}
