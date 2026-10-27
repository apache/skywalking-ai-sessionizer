// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package judge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// PathDecoder reads the files a tool's request names. Tool names are the
// runtime's, so each runtime's adapter has its own and cmd/asz wires it: the
// judge never names a tool.
type PathDecoder func(tool string, input json.RawMessage) []changes.Requested

// Prepare readies a navigation over a narrowed document: it keeps to the
// window, marks the inputs that carried images, and finds the file
// operations, reading the requests of tool steps with decode, which may be
// nil. The run and the navigation server an agent CLI starts both prepare
// theirs here, so the two give the same operations the same ids.
func (n *Nav) Prepare(sel *Selection, records RecordsReader, decode PathDecoder) {
	n.Within(sel)
	n.MarkImages(records)
	n.Decode(decode)
}

// PrepareChecked is Prepare with a reader that tells of its failures: a
// record the images or a tool step's request need that cannot be read stops
// the preparation, where Prepare passes over it.
func (n *Nav) PrepareChecked(sel *Selection, records RecordsReaderChecked, decode PathDecoder) error {
	n.Within(sel)
	if err := n.MarkImagesChecked(records); err != nil {
		return err
	}
	var failed error
	n.readFailed = func(err error) {
		if failed == nil {
			failed = err
		}
	}
	n.Decode(decode)
	n.readFailed = nil
	return failed
}

// Decode finds the conversation's file operations: every file a workspace
// change record captured, and every file the request of a tool step names
// that no record of that step names, read with decode, which may be nil. It runs after
// Within, since a request the document clipped is read whole from its
// record, and only records within the window are read.
func (n *Nav) Decode(decode PathDecoder) { n.ops = operations(n, decode) }

// Operations are the conversation's file operations, in the order they
// landed.
func (n *Nav) Operations() []sessionjudge.Operation { return n.ops }

// operations lists the file operations, ordered by where each landed: a
// change record's files by the record, a request's by its step. A run at one
// head so gives the same ids.
func operations(n *Nav, decode PathDecoder) []sessionjudge.Operation {
	type found struct {
		at sessionflow.Ref
		op sessionjudge.Operation
	}
	var all []found
	// named holds the files each step's change records name, as they name
	// them and joined to their workspace, so a request's file is matched
	// however the request names it.
	named := map[string]map[string]bool{}
	for _, w := range n.atHead {
		base := sessionjudge.Operation{Step: w.Step, Record: w.ID, Source: sessionjudge.SourceChangeRecord,
			Ref: &sessionjudge.Ref{Seq: w.Ref.Seq, Row: w.Ref.Row, Block: w.Ref.Block}}
		if w.Root != nil {
			base.Workspace = w.Root.Path
		}
		// A record whose file list was taken out to fit still says files
		// changed.
		if len(w.Changes) == 0 && w.ChangedFiles != nil && *w.ChangedFiles > 0 {
			op := base
			op.Operation = sessionjudge.OperationUnlisted
			all = append(all, found{w.Ref, op})
		}
		for _, f := range w.Changes {
			op := base
			op.Path, op.Operation, op.Attribution, op.Added, op.Removed = f.Path, f.Operation, f.Attribution, f.Additions, f.Deletions
			all = append(all, found{w.Ref, op})
			if named[w.Step] == nil {
				named[w.Step] = map[string]bool{}
			}
			named[w.Step][f.Path] = true
			if base.Workspace != "" {
				named[w.Step][strings.TrimSuffix(base.Workspace, "/")+"/"+f.Path] = true
			}
		}
	}
	// A request names an attempt. A file a change record of its step names is
	// there already as a change; the others are added, since a capture can
	// name some of a step's files and not others, or none when its list was
	// taken out to fit.
	if decode != nil {
		for id, x := range n.nodes {
			if x.Kind != model.KindTool || x.Ref == nil {
				continue
			}
			for _, p := range decode(x.Name, n.request(x)) {
				if named[id][p.Path] {
					continue
				}
				all = append(all, found{*x.Ref, sessionjudge.Operation{Step: id, Path: p.Path, Operation: p.Operation,
					Source: sessionjudge.SourceToolRequest, Status: observed(n, id),
					Ref: &sessionjudge.Ref{Seq: x.Ref.Seq, Row: x.Ref.Row, Block: x.Ref.Block}}})
			}
		}
	}
	// Steps are found in no fixed order, so two at one place are told apart
	// by their ids; one step's operations keep the order they were read in.
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		switch {
		case refBefore(a.at, b.at):
			return true
		case refBefore(b.at, a.at):
			return false
		}
		return a.op.Step < b.op.Step
	})
	out := make([]sessionjudge.Operation, len(all))
	for i, f := range all {
		f.op.ID = fmt.Sprintf("o%d", i+1)
		out[i] = f.op
	}
	return out
}

// request is a tool step's request whole: the document's text, or, when the
// document clipped it, the part of the record the step stands on.
func (n *Nav) request(x *sessionview.Node) json.RawMessage {
	if len(x.Text) >= x.Bytes {
		return json.RawMessage(x.Text)
	}
	rec, err := n.readable(x.Ref.Seq, x.Ref.Row)
	if err != nil && n.readFailed != nil {
		n.readFailed(err)
	}
	if err != nil || rec == nil {
		return nil
	}
	for pi, p := range rec.Parts {
		if x.Ref.Block != nil && pi != *x.Ref.Block {
			continue
		}
		if len(p.Data) > 0 {
			return p.Data
		}
		return json.RawMessage(p.Text)
	}
	return nil
}

// operationRows are the operations of some talks, or of every one, a row
// each. An operation no step joins is in no talk, and is listed only when
// no talks are asked for.
func (n *Nav) operationRows(only []string) []any {
	want := set(only)
	var rows []any
	for _, op := range n.ops {
		talk := mainTalkOf(n, op.Step)
		if len(want) > 0 && !want[talk] {
			continue
		}
		rows = append(rows, map[string]any{"id": op.ID, "step": op.Step, "talk": talk, "path": op.Path, "operation": op.Operation,
			"source": op.Source, "status": op.Status, "added": op.Added, "removed": op.Removed, "record": op.Record})
	}
	return rows
}
