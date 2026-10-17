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

package metrics

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// executionKey is how an observation is remembered as counted, beside the
// calls the token metric counts. The prefix keeps the two apart.
func executionKey(id string) string { return "execution:" + id }

// deriveExecutions turns one execution file's records into the MCP family's
// points.
//
// One observation is one call counted, once per session by its own id: the
// same line landed again in a later file is the same observation. A record
// is counted in the minute it was observed. Only calls to MCP servers are
// counted. The server is the name the observer gave, the tool the part of
// the runtime's name after the server where that name splits exactly, and
// otherwise the whole name: together they name the target a call reached,
// which a receiver can treat as an endpoint of the agent. The server's
// source is where its configuration came from. Measured on Claude Code
// 2.1.282, every hook after a call carries its duration, failures included,
// so the time divided by the calls of a series is the mean. A record without
// a duration would still count as a call and add no time.
//
// Nothing says whether a tool reads or writes. An MCP server can declare
// that per tool, but measured on Claude Code 2.1.282 the declaration reaches
// neither the model, nor the hooks, nor the transcript, so no label is made
// up for it.
func deriveExecutions(rd *sessiondata.Reader, f io.Reader, h hash.Hash, hdr *sessiondata.Header, since time.Time, ss *sessionState, res *result) (*result, error) {
	source := SourceSubagent
	if hdr.Stream == "main" {
		source = SourceMain
	}
	sums := map[point]int64{}
	here := map[string]bool{}
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		t, ok := recordTime(rec)
		if ok && t.After(res.latest) {
			res.latest = t
		}
		for _, p := range rec.Parts {
			if p.Kind != sessiondata.PartData {
				continue
			}
			r, isExec := execution.Decode(p.Data)
			if !isExec || r.Protocol != execution.ProtocolMCP || r.Server == nil || !ok {
				continue
			}
			key := executionKey(r.ID)
			if ss.Calls[key] || here[key] {
				continue
			}
			here[key] = true
			res.counted = append(res.counted, key)
			minute := t.Truncate(time.Minute)
			if !since.IsZero() && minute.Add(time.Minute).Before(since) {
				continue
			}
			tool := r.ToolName
			if _, split, exact := claudecode.MCPName(r.ToolName); exact {
				tool = split
			}
			s := series{server: r.Server.Name, origin: r.Server.Source, tool: tool, outcome: r.Outcome, source: source}
			s.metric = MCPCalls
			sums[point{series: s, session: hdr.Session, minute: minute}]++
			if r.DurationMS != nil {
				s.metric = MCPDuration
				sums[point{series: s, session: hdr.Session, minute: minute}] += *r.DurationMS
			}
		}
	}
	if _, err := io.Copy(io.Discard, f); err != nil {
		return nil, err
	}
	res.digest = fileDigest(hdr, h)
	points := make([]point, 0, len(sums))
	for k, v := range sums {
		k.value = v
		points = append(points, k)
	}
	res.points = windows(points, res, ss)
	return res, nil
}

// unread counts the execution observations of a session's skeletons that no
// pass counted.
//
// A skeleton keeps an execution record's id, tool, time and outcome, and not
// its protocol or its server. So whether it was a call to an MCP server is not
// known, and its MCP metrics cannot be derived. Metrics are derived only while
// an endpoint is set, so a session removed with none set reaches a deriver
// enabled later with nothing of its executions counted. Nothing records why an
// observation was not counted before its file was removed, so this is a count
// of what is there now: distinct observations a skeleton keeps that are not
// among the counted ones. Some of them may not have been MCP calls.
//
// Only execution files are opened, by their name: a root's skeletons are
// mostly transcripts, and a reader holds a buffer of its own.
func unread(files []storage.LandedFile, ss *sessionState) (int, error) {
	ids := map[string]bool{}
	for _, lf := range files {
		if !lf.Skeleton || !strings.HasPrefix(filepath.Base(lf.Path), string(sessiondata.KindExecution)+"-") {
			continue
		}
		if err := executionIDs(lf.Path, ids); err != nil {
			return 0, fmt.Errorf("%s: %w", lf.Path, err)
		}
	}
	n := 0
	for id := range ids {
		if !ss.Calls[executionKey(id)] {
			n++
		}
	}
	return n, nil
}

// executionIDs adds the observation id of every execution record a file
// holds.
func executionIDs(path string, ids map[string]bool) error {
	f, err := sessiondata.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rd, err := sessiondata.NewReader(f)
	if err != nil {
		return err
	}
	if rd.Header().Kind != sessiondata.KindExecution {
		return nil
	}
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, p := range rec.Parts {
			if p.Kind != sessiondata.PartData {
				continue
			}
			if r, ok := execution.Decode(p.Data); ok && r.ID != "" {
				ids[r.ID] = true
			}
		}
	}
}
