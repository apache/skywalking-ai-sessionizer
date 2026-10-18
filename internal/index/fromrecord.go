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
	"strconv"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
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
	seq, row uint32) (Entry, []Block, *Body) {

	in := ix.Strings
	e := Entry{
		Seq: seq, Row: row, Ord: rec.Ord,
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
	if e.Kind == KindProviderBody {
		// Its parts are pieces of a body, never change records, and a block
		// per part is all a round needs to count them.
		blocks := make([]Block, len(rec.Parts))
		for i := range blocks {
			blocks[i] = Block{Ord: uint32(i), Kind: BlockOther}
		}
		return e, blocks, bodyOf(in, rec)
	}
	return e, blocksOf(in, rec), nil
}

// bodyOf reads the join keys off a provider body's manifest, which is the
// record's last part. A record whose manifest does not read is indexed like any
// other record and joins to nothing: the file is still evidence, and a reader
// that wants the body will say why it cannot have it.
func bodyOf(in *Interner, rec *sessiondata.Record) *Body {
	m, err := providerbody.ManifestOf(rec)
	if err != nil {
		return nil
	}
	b := &Body{Request: in.ID(m.Request), Previous: in.ID(m.PreviousRequest)}
	switch m.Role {
	case providerbody.RoleRequest:
		b.Role = BodyRoleRequest
	case providerbody.RoleResponse:
		b.Role = BodyRoleResponse
	}
	historyOf(in, m, b)
	return b
}

// historyOf reads the manifest's history fields into the body. A value that
// does not read as the format page says leaves the field unavailable rather
// than guessed at: a digest that is not sixteen hexadecimal characters is not
// one, whatever it holds.
func historyOf(in *Interner, m *providerbody.Manifest, b *Body) {
	if m.HistoryForm == "" {
		return
	}
	b.Form = in.ID(m.HistoryForm)
	if m.System != "" {
		if _, ok := messageOf(providerbody.Entry{Digest: m.System}); ok {
			b.System = in.ID(m.System)
		} else {
			b.SystemUnknown = true
		}
	}
	// A thread no rule describes says the request holds something unknown,
	// so its list cannot be read as its history.
	threadUnknown := false
	if t := m.Thread; t != nil {
		switch {
		case t.Type == providerbody.ThreadCreate:
			b.Thread = ThreadCreate
		case t.Type == providerbody.ThreadContinue && t.Previous != "" && len(t.Previous) <= providerbody.MaxPrevious:
			b.Thread, b.ThreadPrevious = ThreadContinue, in.ID(t.Previous)
		default:
			threadUnknown = true
		}
	}
	if l := m.Messages; l != nil {
		unavailable := func(why string, count int) {
			b.List, b.ListWhy, b.ListCount, b.messages = ListUnavailable, in.ID(why), uint32(count), ""
		}
		entries := l.Entries
		switch {
		case l.Unavailable != "":
			unavailable(l.Unavailable, l.Count)
		case l.Relative():
			b.List, b.ListBase, b.ListKeep = ListRelative, in.ID(l.Base), uint32(l.Keep)
			entries = l.Add
		default:
			b.List = ListWhole
		}
		if b.List != ListUnavailable {
			encoded := make([]byte, 0, len(entries)*messageWidth)
			buf := make([]byte, messageWidth)
			for _, e := range entries {
				msg, ok := messageOf(e)
				if !ok {
					unavailable(providerbody.UnrecognisedShape, 0)
					encoded = nil
					break
				}
				encodeMessage(buf, &msg)
				encoded = append(encoded, buf...)
			}
			b.messages = string(encoded)
		}
		if threadUnknown {
			unavailable(providerbody.UnrecognisedShape, 0)
		}
	}
	if r := m.Reply; r != nil {
		if r.Unavailable != "" {
			b.Reply, b.ReplyWhy = ReplyUnavailable, in.ID(r.Unavailable)
		} else if msg, ok := messageOf(providerbody.Entry{ID: r.ID, Digest: r.Digest}); ok {
			b.Reply, b.ReplyHasID, b.ReplyID, b.ReplyDigest = ReplyEntry, msg.HasID, msg.ID, msg.Digest
		} else {
			b.Reply, b.ReplyWhy = ReplyUnavailable, in.ID(providerbody.UnrecognisedShape)
		}
	}
}

// messageOf turns an entry's two digests into numbers.
func messageOf(e providerbody.Entry) (Message, bool) {
	d, err := strconv.ParseUint(e.Digest, 16, 64)
	if err != nil || len(e.Digest) != 16 {
		return Message{}, false
	}
	msg := Message{Digest: d}
	if e.ID != "" {
		id, err := strconv.ParseUint(e.ID, 16, 64)
		if err != nil || len(e.ID) != 16 {
			return Message{}, false
		}
		msg.HasID, msg.ID = true, id
	}
	return msg, true
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
	case sessiondata.KindChanges:
		return KindChanges
	case sessiondata.KindProviderBody:
		return KindProviderBody
	case sessiondata.KindExecution:
		return KindExecution
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
	"synthetic":             FlagSynthetic,
	"context_reset":         FlagEpochBoundary,
	"reset_summary":         FlagEpochSummary,
	"finished":              FlagStopReason,
	"external_input":        FlagExternalInput,
	"injected":              FlagInjection,
	"child_result":          FlagChildResult,
	"error":                 FlagError,
	"launch_ack":            FlagLaunchAck,
	"turn_duration":         FlagTurnDuration,
	"command":               FlagCommand,
	"notice":                FlagNotice,
	"auxiliary":             FlagAuxiliary,
	model.ClassInternalCall: FlagInternalCall,
	model.ClassTurnCall:     FlagTurnCall,
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
		b := Block{Ord: uint32(i)}
		switch p.Kind {
		case sessiondata.PartCall:
			b.Kind, b.ToolID, b.Name = BlockToolUse, in.ID(p.ID), in.ID(p.Name)
			b.Server, b.ServerTool = in.ID(p.Server), in.ID(p.ServerTool)
		case sessiondata.PartResult:
			b.Kind, b.ToolID = BlockToolResult, in.ID(p.Of)
		case sessiondata.PartText:
			b.Kind = BlockText
		case sessiondata.PartReasoning:
			b.Kind = BlockThinking
		case sessiondata.PartData:
			b.Kind = BlockOther
			if r, ok := changes.Decode(p.Data); ok {
				// Its identity, the way the view tells records apart: one call
				// two producers both recorded is two records, a record landed
				// twice is one. Its lines, so a round can total them.
				b.Kind, b.Name = BlockChanges, in.ID(r.CapturedBy+"|"+r.ID)
				for _, fc := range r.Changes {
					if fc.Additions != nil {
						b.Adds += uint32(*fc.Additions)
					}
					if fc.Deletions != nil {
						b.Dels += uint32(*fc.Deletions)
					}
				}
			}
		default:
			b.Kind = BlockOther
		}
		out = append(out, b)
	}
	return out
}
