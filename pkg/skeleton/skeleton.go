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

// Package skeleton writes the file that takes a landed file's place when
// retention removes it: Session Data that keeps the header, every record and
// every part in its place, and only the fields a conversation's structure is
// built from. The index rebuilt from a skeleton is the index rebuilt from the
// landed file, so a conversation continues after a removal exactly as it
// would with none.
package skeleton

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Write writes the skeleton of the landed file data to w, compressed with
// gzip. removed gives when and by which removal it is written, and what the
// receiver confirmed; the landed file's digest, size and record count are
// taken from data, so they cannot disagree with it.
//
// The landed file is read whole, closing line included, so a file that does
// not read never becomes a skeleton that does.
func Write(w io.Writer, data []byte, removed sessiondata.Removed) error {
	r, err := sessiondata.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	hdr := r.Header()
	if hdr.Removed != nil {
		return errors.New("skeleton: the file is a skeleton already")
	}
	var records []*sessiondata.Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("skeleton: record %d: %w", len(records)+1, err)
		}
		records = append(records, rec)
	}
	sum := sha256.Sum256(data)
	removed.Digest, removed.Size, removed.Records = hex.EncodeToString(sum[:]), int64(len(data)), len(records)
	hdr.Removed = &removed

	zw := gzip.NewWriter(w)
	sw, err := sessiondata.NewWriter(zw, &hdr)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if err := sw.Write(Of(&hdr, rec)); err != nil {
			return err
		}
	}
	if err := sw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// Of is the skeleton of one record of a file with header hdr. It keeps the
// record's place in its source, its identifiers, its time, trigger, flags,
// usage and model, and every part in its place, because a part's position is
// its block number and ids such as msg/<seq>/<row>:<block> are built on it.
//
// A part keeps its kind, its size and whether it failed; a call keeps its id,
// name and server, and a result the call it answers. Text, data and media go,
// and the state says removed. A part too large to land says so still: it held
// nothing to remove, and a provider body cut later must not take its piece
// for one the store can give. Three kinds of data are cut down instead, each
// only after its own decoder accepted it, so data that did not decode never
// becomes data that does: a changes record keeps what the index counts and
// what the page joins and orders it by, an execution record keeps what the
// page joins and orders it by, and a provider body keeps its manifest without
// the literal strings and the digest of every piece.
func Of(hdr *sessiondata.Header, rec *sessiondata.Record) *sessiondata.Record {
	out := &sessiondata.Record{
		Ord: rec.Ord, Off: rec.Off, Bytes: rec.Bytes,
		ID: rec.ID, Parent: rec.Parent, Call: rec.Call, Run: rec.Run, Continues: rec.Continues,
		Tool: rec.Tool, Child: rec.Child, Batch: rec.Batch, Label: rec.Label, StartedBy: rec.StartedBy,
		From: rec.From, Time: rec.Time, Trigger: rec.Trigger, Flags: rec.Flags,
		Usage: rec.Usage, Model: rec.Model, Dropped: rec.Dropped,
	}
	body := hdr.Kind == sessiondata.KindProviderBody
	if body {
		// verify reads a body's record digest.
		out.Sha = rec.Sha
	}
	if len(rec.Parts) == 0 {
		return out
	}
	m, manifestErr := providerbody.ManifestOf(rec)
	last := len(rec.Parts) - 1
	out.Parts = make([]sessiondata.Part, len(rec.Parts))
	for i, p := range rec.Parts {
		q := sessiondata.Part{Kind: p.Kind, Bytes: p.Bytes, Failed: p.Failed, State: model.ContentRemoved, SHA256: p.SHA256}
		if p.State == model.ContentTooLarge {
			q.State = p.State
		}
		switch p.Kind {
		case sessiondata.PartCall:
			q.ID, q.Name, q.Server, q.ServerTool = p.ID, p.Name, p.Server, p.ServerTool
		case sessiondata.PartResult:
			q.Of = p.Of
		case sessiondata.PartData:
			switch {
			case body && i == last && manifestErr == nil:
				q.Data = manifest(m)
			case body && i < last:
				// A body's piece is known by the digest of its data, or by
				// the digest a part too large to land kept.
				if p.State != model.ContentTooLarge {
					q.SHA256 = digest(p.Data)
				}
			default:
				if r, ok := changes.Decode(p.Data); ok {
					q.Data = changesOf(r)
				} else if r, ok := execution.Decode(p.Data); ok {
					q.Data = executionOf(r)
				}
			}
		}
		out.Parts[i] = q
	}
	return out
}

// manifest is a body's manifest without its literal strings, written from
// the manifest as a reader decodes it, so the skeleton holds exactly what a
// session checks the body by and the index reads, whatever spelling the
// landed file used. A literal is text of the body, so each segment loses its
// lit and keeps the part, the piece or the earlier body it names; a segment
// left empty goes. A session holding the skeleton takes it or not as it did
// the body.
func manifest(m *providerbody.Manifest) json.RawMessage {
	kept := *m
	// A body whose segments were all literal keeps an empty list, not none:
	// a reader that requires the list, as Horizon's renderer does, refuses a
	// manifest without one, and must judge the skeleton as it judged the body.
	kept.Segments = nil
	if m.Segments != nil {
		kept.Segments = []providerbody.Segment{}
	}
	for _, seg := range m.Segments {
		seg.Lit = ""
		if seg.Part != nil || seg.Piece != "" || seg.Copy != nil {
			kept.Segments = append(kept.Segments, seg)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(kept) != nil {
		return nil
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

// changesOf is what a changes record keeps: what tells it from another
// record and joins it to its step, when it was observed, and for each file
// its path, its operation, its two sides by presence, size and digest, and its
// lines, which is what the page groups and counts the files by. The hunks go,
// and with them the windows, the policy and the outcome. A diff that was
// available says unavailable, since its lines are not kept.
func changesOf(r *changes.Record) json.RawMessage {
	out := changes.Record{Schema: r.Schema, ID: r.ID, CapturedBy: r.CapturedBy, Tool: r.Tool, ToolName: r.ToolName,
		Time: r.Time, Basis: r.Basis, Root: r.Root, Coverage: r.Coverage, ChangedFiles: r.ChangedFiles,
		Changes: make([]changes.FileChange, len(r.Changes))}
	for i, c := range r.Changes {
		diff := c.Diff
		if diff == changes.DiffAvailable {
			diff = changes.DiffUnavailable
		}
		out.Changes[i] = changes.FileChange{Path: c.Path, Operation: c.Operation, Before: c.Before, After: c.After,
			Diff: diff, Attribution: c.Attribution, Additions: c.Additions, Deletions: c.Deletions}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return data
}

// executionRecord is what an execution record keeps: its own id, which
// tells it from another observation of the same call; the tool it joins to;
// when it was observed, which orders it among the others; and how the call
// ended, which the page draws on the step.
type executionRecord struct {
	Schema  string `json:"schema"`
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Time    string `json:"time"`
	Outcome string `json:"outcome"`
}

func executionOf(r *execution.Record) json.RawMessage {
	data, err := json.Marshal(executionRecord{Schema: r.Schema, ID: r.ID, Tool: r.Tool, Time: r.Time, Outcome: r.Outcome})
	if err != nil {
		return nil
	}
	return data
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
