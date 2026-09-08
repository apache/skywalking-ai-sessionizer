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
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// workspaceChanges gathers every change record the session carries and
// joins each to its step.
//
// Two places hold them. A result record of an editing tool carries the
// runtime's own patch as a data part beside the raw result, and those
// records are already in hand: a tool step reads its result. A changes
// file the plugin's adapter landed holds one record per line, and those
// files are read here, whole, since they are small. The join is the
// tool-use id, which the change record names and the step's call part
// carries; nothing is matched by time.
func (c *Conversation) workspaceChanges(landed []storage.LandedFile, recs map[[2]uint64]*sessiondata.Record) []sessionview.WorkspaceChange {
	// Which step carries which tool-use id, from the call part each tool
	// step points at.
	stepOf := map[string]string{}
	for _, n := range c.View.Nodes {
		if (n.Kind != model.KindTool && n.Kind != model.KindAgentCall) || n.Ref == nil {
			continue
		}
		rec := recs[[2]uint64{n.Ref.Seq, n.Ref.Row}]
		if rec == nil {
			continue
		}
		if p := partAt(rec, n.Ref.Block); p != nil && p.Kind == sessiondata.PartCall && p.ID != "" {
			stepOf[p.ID] = n.ID
		}
	}

	var out []sessionview.WorkspaceChange
	seen := map[string]bool{}
	add := func(r *changes.Record, source string, ref sessionflow.Ref) {
		if seen[r.ID] {
			return // a line landed twice by an interrupted pass
		}
		seen[r.ID] = true
		out = append(out, sessionview.WorkspaceChange{Step: stepOf[r.Tool], Source: source, Ref: ref, Record: *r})
	}

	// The runtime's own patches, on the result records the tool steps read.
	for _, n := range c.View.Nodes {
		if n.Kind != model.KindTool || len(n.Refs) < 2 {
			continue
		}
		for i := 1; i < len(n.Refs); i++ {
			ref := n.Refs[i]
			rec := recs[[2]uint64{ref.Seq, ref.Row}]
			if rec == nil {
				continue
			}
			for b, p := range rec.Parts {
				if p.Kind != sessiondata.PartData {
					continue
				}
				if r, ok := changes.Decode(p.Data); ok {
					block := b
					add(r, sessionview.SourceRuntime, sessionflow.Ref{Seq: ref.Seq, Row: ref.Row, Block: &block})
				}
			}
		}
	}

	// The plugin's records, from the files its adapter landed.
	for _, lf := range landed {
		if !strings.HasPrefix(filepath.Base(lf.Path), string(sessiondata.KindChanges)+"-") {
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
			for b, p := range rec.Parts {
				if p.Kind != sessiondata.PartData {
					continue
				}
				if r, ok := changes.Decode(p.Data); ok {
					block := b
					add(r, sessionview.SourcePlugin, sessionflow.Ref{Seq: lf.Seq, Row: row, Block: &block})
				}
			}
		}
		f.Close()
	}

	// By time, then by id, so the same files give the same list. A
	// runtime-reported record and a plugin record for one tool both stay:
	// the runtime's is listed first, and a viewer showing one prefers it.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		if out[i].Source != out[j].Source {
			return out[i].Source == sessionview.SourceRuntime
		}
		return out[i].ID < out[j].ID
	})
	if out == nil {
		out = []sessionview.WorkspaceChange{}
	}
	return out
}

// partAt returns the part a reference names, or the only part when the
// reference names none.
func partAt(rec *sessiondata.Record, block *int) *sessiondata.Part {
	if block != nil && *block < len(rec.Parts) {
		return &rec.Parts[*block]
	}
	if len(rec.Parts) == 1 {
		return &rec.Parts[0]
	}
	return nil
}

// annotateChanges writes each step's change ids onto the step, in the
// order the document lists the records.
func annotateChanges(nodes []sessionview.Node, byStep map[string][]string) {
	for i := range nodes {
		if ids := byStep[nodes[i].ID]; len(ids) > 0 {
			nodes[i].Changes = ids
		}
		annotateChanges(nodes[i].Children, byStep)
	}
}
