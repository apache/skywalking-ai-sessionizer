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
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// toolExecutions gathers every execution record the session's execution
// files carry and joins each to the step of the call it observed.
//
// The join is the tool-use id the record names, which the step's call part
// carries; nothing is matched by name or time. One call can have several
// records, one per observation, so a step lists them all. A record is kept
// once by its own id: the same record landed twice, by a hook handed its
// event again or by an interrupted pass, is one record. A record whose call is not a step of the document keeps no
// step rather than being dropped.
func (c *Conversation) toolExecutions(landed []storage.LandedFile, recs map[[2]uint64]*sessiondata.Record) []sessionview.ToolExecution {
	stepOf := c.stepsByToolUse(recs)
	var out []sessionview.ToolExecution
	seen := map[string]bool{}
	for _, lf := range landed {
		if !strings.HasPrefix(filepath.Base(lf.Path), string(sessiondata.KindExecution)+"-") {
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
				r, ok := execution.Decode(p.Data)
				if !ok || seen[r.ID] {
					continue
				}
				seen[r.ID] = true
				block := b
				out = append(out, sessionview.ToolExecution{Step: stepOf[r.Tool],
					Ref: sessionflow.Ref{Seq: lf.Seq, Row: row, Block: &block}, Record: *r})
			}
		}
		f.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return c.executionBefore(&out[i], &out[j]) })
	if out == nil {
		out = []sessionview.ToolExecution{}
	}
	return out
}

// executionBefore orders tool executions: by time, then by where each was
// read, so the same files give the same list.
func (c *Conversation) executionBefore(a, b *sessionview.ToolExecution) bool {
	if byTime := compareTimes(a.Time, b.Time); byTime != 0 {
		return byTime < 0
	}
	return comparePositions(c.pointOf(&a.Ref), c.pointOf(&b.Ref)) < 0
}

// annotateExecutions writes each step's execution record ids onto the step,
// in the order the document lists the records.
func annotateExecutions(nodes []sessionview.Node, byStep map[string][]string) {
	for i := range nodes {
		if ids := byStep[nodes[i].ID]; len(ids) > 0 {
			nodes[i].Executions = ids
		}
		annotateExecutions(nodes[i].Children, byStep)
	}
}
