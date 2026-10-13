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
	"errors"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// What a skeleton may have kept is counted once for each body and each piece,
// however many times its records name them, and by the sizes they say they
// hold, before anything is read: a record landed twice, as an interrupted pass
// lands one, does not count its body twice against the budget.
func TestASkeletonsKeptBytesAreCountedOnce(t *testing.T) {
	piece := strings.Repeat("e", 64)
	m := `{"schema":"` + providerbody.Schema + `","role":"request","sha256":"` + strings.Repeat("a", 64) +
		`","bytes":1000,"depth":0,"segments":[{"part":0}]}`
	rec := func() *sessiondata.Record {
		return &sessiondata.Record{Ord: 1, ID: "req-1", Parts: []sessiondata.Part{
			{Kind: sessiondata.PartData, State: model.ContentRemoved, Bytes: 300, SHA256: piece},
			{Kind: sessiondata.PartData, State: model.ContentRemoved, Bytes: 300, SHA256: piece},
			{Kind: sessiondata.PartData, Data: []byte(m), State: model.ContentRemoved, Bytes: len(m)},
		}}
	}
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Seq: 1, At: "2026-10-01T00:00:00Z",
		Kind: sessiondata.KindProviderBody, Adapter: "test/0", Dialect: "test/1", Src: ".", Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := w.Write(rec()); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := keptWanted(buf.Bytes(), has{tips: true, pieces: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(want.bodies) != 1 || len(want.pieces) != 1 || want.bytes != 1300 {
		t.Fatalf("wanted %d bodies and %d pieces of %d bytes, want 1, 1 and 1300", len(want.bodies), len(want.pieces), want.bytes)
	}
	// What the store does not keep counts nothing: a removed body that was
	// no chain tip names its size and keeps no bytes.
	want, err = keptWanted(buf.Bytes(), has{pieces: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(want.bodies) != 0 || want.bytes != 300 {
		t.Fatalf("with no tip kept, wanted %d bodies of %d bytes, want none and 300", len(want.bodies), want.bytes)
	}
	// A store that cannot be looked at is an error, never a store that keeps
	// nothing: the page would say the body was removed.
	failing := errors.New("permission denied")
	for _, h := range []has{{tipErr: failing}, {tips: true, pieceErr: failing}} {
		if _, err := keptWanted(buf.Bytes(), h); !errors.Is(err, failing) {
			t.Fatalf("a store whose %+v cannot be looked at: %v", h, err)
		}
	}
}

// has is a store that has every tip, every piece, or neither, or cannot be
// looked at.
type has struct {
	tips, pieces     bool
	tipErr, pieceErr error
}

func (h has) HasTip(string) (bool, error)   { return h.tips, h.tipErr }
func (h has) HasPiece(string) (bool, error) { return h.pieces, h.pieceErr }
