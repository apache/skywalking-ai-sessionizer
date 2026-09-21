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
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"sort"
	"strings"
)

// Operation is one arrival of evidence about one run.
//
// The client names each part <op>.<run id>[.<field>], sending the run's
// envelope under the bare name and anything large beside it under its own.
// An Operation gathers those back into one thing while keeping each field's
// bytes as they arrived, because a landed record proves its source by the
// digest of those bytes and not of something re-encoded.
type Operation struct {
	// Op is "post" for a run's first arrival and "patch" for a later one.
	// The client sends both, and a run that finished before the batch went
	// out arrives as a post that is already complete.
	Op string
	// RunID is the run this arrival concerns. It is shared by every arrival
	// about that run, which is what makes it the call rather than the
	// receipt.
	RunID string
	// Envelope is the run object: id, trace_id, parent_run_id, dotted_order,
	// run_type, name, start_time, end_time, tags, session_name.
	Envelope json.RawMessage
	// Fields are the parts sent out of band, by name: inputs, outputs,
	// events, extra, serialized, error. Bytes as received.
	Fields map[string]json.RawMessage
	// Order is the part's position in the request, counting from 1. It is
	// what a landed record reports as its position in its source, since a
	// request has no lines to count.
	Order int
	// Bytes is what this arrival took on the wire, envelope and fields
	// together.
	Bytes int
}

// Feedback and Attachment parts are accepted and counted, never landed.
// Feedback is a score on a run rather than evidence of the conversation, and
// an attachment needs a measurement of how large and how often before it has a
// place. Dropping them silently would be the wrong kind of quiet, so the
// receiver counts them and the status says so.
const (
	partFeedback   = "feedback"
	partAttachment = "attachment"
)

// Request is everything one accepted request carried.
type Request struct {
	Operations []Operation
	// Feedback and Attachments count what was accepted and not landed.
	Feedback    int
	Attachments int
}

// KnownFields are the field names the client sends out of band.
//
// A name outside this set is kept anyway, under its own key: a client that
// grows a field must not lose it here, and the dialect can say it did not
// understand something rather than the receiver pretending it never came. The
// set is here so a reader can tell the two apart.
var KnownFields = map[string]bool{
	"inputs": true, "outputs": true, "events": true,
	"extra": true, "serialized": true, "error": true,
}

// UnknownFields names the parts of a request whose field this build does not
// know. It is what a status line reports when a client has grown something.
func (r *Request) UnknownFields() []string {
	seen := map[string]bool{}
	var out []string
	for _, op := range r.Operations {
		for name := range op.Fields {
			if !KnownFields[name] && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ParseMultipart reads a multipart ingest body into the operations it carries.
//
// The part name is the whole protocol here: "post.<uuid>" is a run's envelope,
// "post.<uuid>.inputs" is that run's inputs, and the two must end up in one
// Operation however they were ordered in the body.
func ParseMultipart(body io.Reader, contentType string) (*Request, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("langsmith: content type: %w", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("langsmith: multipart body with no boundary")
	}
	reader := multipart.NewReader(body, boundary)
	// open holds the arrival a run's parts are still going into. A body can
	// carry more than one update to a run - the client keeps them apart when
	// the run's start is not in the same batch - and keying only by the run
	// let them overwrite each other, so an envelope and any field they
	// shared were lost before anything read them.
	open := map[string]*Operation{}
	var arrivals []*Operation
	out := &Request{}
	position := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("langsmith: read part: %w", err)
		}
		position++
		name := part.FormName()
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return nil, fmt.Errorf("langsmith: read part %q: %w", name, err)
		}
		kind, runID, field, ok := splitPartName(name)
		if !ok {
			return nil, fmt.Errorf("langsmith: part %q is not named <op>.<run id>[.<field>]", name)
		}
		switch kind {
		case partFeedback:
			out.Feedback++
			continue
		case partAttachment:
			out.Attachments++
			continue
		case "post", "patch":
		default:
			return nil, fmt.Errorf("langsmith: part %q names an unknown operation %q", name, kind)
		}
		// An arrival is its parts taken together. A part that names
		// something this arrival already has begins the next one.
		key := kind + "." + runID
		op := open[key]
		switch {
		case op == nil:
		case field == "" && len(op.Envelope) > 0:
			op = nil
		case field != "":
			if _, already := op.Fields[field]; already {
				op = nil
			}
		}
		if op == nil {
			op = &Operation{Op: kind, RunID: runID, Fields: map[string]json.RawMessage{},
				Order: position}
			open[key] = op
			arrivals = append(arrivals, op)
		}
		if field == "" {
			op.Envelope = json.RawMessage(data)
		} else {
			op.Fields[field] = json.RawMessage(data)
		}
		op.Bytes += len(data)
	}
	for _, op := range arrivals {
		if len(op.Envelope) == 0 {
			// A field with no envelope is a part naming a run this request
			// never described. Keeping it would mean landing a record with no
			// run to attach it to.
			return nil, fmt.Errorf("langsmith: %s.%s carries fields but no run", op.Op, op.RunID)
		}
		out.Operations = append(out.Operations, *op)
	}
	sort.SliceStable(out.Operations, func(i, j int) bool {
		return out.Operations[i].Order < out.Operations[j].Order
	})
	return out, nil
}

// splitPartName reads "<op>.<run id>[.<field>]".
//
// A run id holds no dot and a field name holds none either, so the first two
// dots are the whole grammar. Anything else is refused rather than guessed at:
// a name this does not understand is a client that changed, which has to be
// loud.
func splitPartName(name string) (kind, runID, field string, ok bool) {
	first := strings.IndexByte(name, '.')
	if first <= 0 {
		return "", "", "", false
	}
	kind, rest := name[:first], name[first+1:]
	second := strings.IndexByte(rest, '.')
	if second < 0 {
		if rest == "" {
			return "", "", "", false
		}
		return kind, rest, "", true
	}
	runID, field = rest[:second], rest[second+1:]
	if runID == "" || field == "" {
		return "", "", "", false
	}
	return kind, runID, field, true
}
