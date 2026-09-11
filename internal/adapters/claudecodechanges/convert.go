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

package claudecodechanges

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Convert turns one line the plugin wrote into a Session Data record.
//
// The line is already in the model's vocabulary, so the conversion is a
// framing: the record's identity, the tool it joins to and its time are
// lifted onto the record, and the line travels whole as one data part,
// byte for byte. A line that is not a change record still lands, as an
// unknown part that keeps the bytes, so nothing the plugin wrote is lost
// to a version of this adapter that does not understand it.
func Convert(ord, off uint64, payload []byte) *sessiondata.Record {
	sum := sha256.Sum256(payload)
	rec := &sessiondata.Record{
		Ord: ord, Off: off,
		Sha:   hex.EncodeToString(sum[:])[:12],
		Bytes: len(payload),
	}
	r, ok := changes.Decode(payload)
	if !ok {
		rec.Parts = []sessiondata.Part{unknownPart(payload, "the line is not a changes/1 record")}
		return rec
	}
	if err := r.Validate(); err != nil {
		rec.Parts = []sessiondata.Part{unknownPart(payload, err.Error())}
		return rec
	}
	rec.ID, rec.Tool, rec.Time = r.ID, r.Tool, r.Time
	rec.Parts = []sessiondata.Part{{
		Kind: sessiondata.PartData, Data: json.RawMessage(payload),
		State: model.ContentAvailable, Bytes: len(payload),
	}}
	return rec
}

// unknownPart keeps a line the adapter could not read, every byte of it.
// SetRaw is what keeps a byte that is not valid UTF-8, which a plain JSON
// string would replace. A write cut short and joined to the next line is
// one way such a byte reaches this adapter.
func unknownPart(b []byte, why string) sessiondata.Part {
	p := sessiondata.Part{Kind: sessiondata.PartUnknown, Text: why, State: model.ContentAvailable, Bytes: len(b)}
	p.SetRaw(b)
	return p
}
