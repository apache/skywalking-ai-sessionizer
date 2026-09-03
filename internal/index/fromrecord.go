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

package index

import (
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// FromRecord builds an index entry from one Session Data record.
//
// This is the whole of index construction, and there is no dialect in it. A
// record arrives already converted, with role-named identifiers and content
// broken into parts, so the index reads the same fields regardless of which
// agent produced the conversation.
//
// It used to be otherwise: the index was built by an adapter reading raw
// payloads, which meant every runtime needed its own index-building code and
// the index could only be rebuilt by whichever adapter wrote it.
func FromRecord(ix *Index, hdr *sessiondata.Header, rec *sessiondata.Record,
	seq, row uint32) (Entry, []Block) {

	in := ix.Strings
	e := Entry{
		Seq: seq, Row: row,
		Stream: in.ID(hdr.Stream), Batch: in.ID(batchOf(hdr, rec)),
		Kind: kindOf(hdr, rec),

		Record:    in.ID(rec.ID),
		Parent:    in.ID(rec.Parent),
		Call:      in.ID(rec.Call),
		Run:       in.ID(rec.Run),
		Continues: in.ID(rec.Continues),
		Tool:      in.ID(rec.Tool),
		Child:     in.ID(rec.Child),
		StartedBy: in.ID(rec.StartedBy),
		Label:     in.ID(rec.Label),

		Trigger: triggerOf(rec.Trigger),
		Flags:   flagsOf(rec.Flags),
	}
	if rec.Time != "" {
		if t, err := time.Parse(time.RFC3339Nano, rec.Time); err == nil {
			e.TS = t.UnixNano()
		}
	}
	return e, blocksOf(in, rec)
}

// batchOf takes the batch from the record when it names one, and from the file
// otherwise.
//
// Both happen. A journal's records belong to the batch its file was collected
// from; a launch result names a batch its own file knows nothing about, and
// that one is the link between a parent's call and the children it started.
func batchOf(hdr *sessiondata.Header, rec *sessiondata.Record) string {
	if rec.Batch != "" {
		return rec.Batch
	}
	return hdr.Batch
}

// kindOf classifies a record.
//
// Two of the three answers come from the FILE rather than the record: a sidecar
// and a run journal are known by where they were collected from, not by
// anything inside them.
func kindOf(hdr *sessiondata.Header, rec *sessiondata.Record) Kind {
	switch hdr.Kind {
	case sessiondata.KindAgentMeta:
		return KindMeta
	case sessiondata.KindJournal:
		return KindJournal
	case sessiondata.KindWorkflowManifest:
		return KindManifest
	case sessiondata.KindWorkflowScript:
		return KindScript
	}
	switch rec.From {
	case sessiondata.FromAgent:
		return KindAssistant
	case sessiondata.FromExternal:
		return KindUser
	case sessiondata.FromRuntime:
		return KindSystem
	}
	return KindUnknown
}

func triggerOf(t string) Trigger {
	switch t {
	case model.TriggerExternal:
		return TriggerExternal
	case model.TriggerNotification:
		return TriggerNotification
	case model.TriggerUnknown:
		return TriggerOther
	}
	return TriggerNone
}

// flagNames is the mapping the adapter writes and the index reads back. It is
// one table so the two cannot disagree.
var flagNames = map[string]Flags{
	"synthetic":      FlagSynthetic,
	"context_reset":  FlagEpochBoundary,
	"reset_summary":  FlagEpochSummary,
	"finished":       FlagStopReason,
	"external_input": FlagExternalInput,
	"injected":       FlagInjection,
	"child_result":   FlagChildResult,
	"error":          FlagError,
	"launch_ack":     FlagLaunchAck,
	"turn_duration":  FlagTurnDuration,
	"command":        FlagCommand,
	"notice":         FlagNotice,
}

// FlagName returns the written form of one flag.
func FlagName(f Flags) string {
	for name, bit := range flagNames {
		if bit == f {
			return name
		}
	}
	return ""
}

func flagsOf(names []string) Flags {
	var f Flags
	for _, n := range names {
		f |= flagNames[n]
	}
	return f
}

// blocksOf turns a record's parts into joinable blocks.
//
// Every part becomes a block, in order, because a part's position is how a step
// is located inside a record and skipping one would shift every part after it.
func blocksOf(in *Interner, rec *sessiondata.Record) []Block {
	if len(rec.Parts) == 0 {
		return nil
	}
	out := make([]Block, 0, len(rec.Parts))
	for i, p := range rec.Parts {
		b := Block{Ord: uint16(i)}
		switch p.Kind {
		case sessiondata.PartCall:
			b.Kind, b.ToolID, b.Name = BlockToolUse, in.ID(p.ID), in.ID(p.Name)
		case sessiondata.PartResult:
			b.Kind, b.ToolID = BlockToolResult, in.ID(p.Of)
		case sessiondata.PartText:
			b.Kind = BlockText
		case sessiondata.PartReasoning:
			b.Kind = BlockThinking
		case sessiondata.PartData:
			b.Kind = BlockOther
		default:
			b.Kind = BlockOther
		}
		out = append(out, b)
	}
	return out
}
