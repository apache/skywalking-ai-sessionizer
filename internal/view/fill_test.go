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
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// A step that stands on a whole record says too_large when a part of the
// record was too large to land, as a screenshot pasted beside a sentence is.
// Its text is the sentence, which landed. A record of one part reads through
// that part, so this is the case of several.
func TestAWholeRecordStepSaysAPartWasTooLarge(t *testing.T) {
	rec := &sessiondata.Record{Parts: []sessiondata.Part{
		{Kind: sessiondata.PartText, Text: "look at this", State: model.ContentAvailable, Bytes: 12},
		{Kind: sessiondata.PartMedia, Media: "image/png", State: model.ContentTooLarge, Bytes: 4_628_610, SHA256: "ab"},
	}}
	var s step
	fill(&s, rec, nil)
	if s.State != model.ContentTooLarge || s.Text != "look at this" {
		t.Fatalf("the step says %q with text %q, want too_large and the sentence", s.State, s.Text)
	}
	whole := &sessiondata.Record{Parts: []sessiondata.Part{
		{Kind: sessiondata.PartText, Text: "look at this", State: model.ContentAvailable},
		{Kind: sessiondata.PartText, Text: "and this", State: model.ContentAvailable},
	}}
	var w step
	fill(&w, whole, nil)
	if w.State != "" {
		t.Fatalf("a whole record that landed whole says %q", w.State)
	}
}

// A step on a whole record of several parts that retention removed says so,
// and removed wins over too large: the skeleton keeps a part's state no more,
// and every part of it was removed.
func TestAWholeRecordStepSaysItWasRemoved(t *testing.T) {
	rec := &sessiondata.Record{Parts: []sessiondata.Part{
		{Kind: sessiondata.PartText, State: model.ContentRemoved, Bytes: 12},
		{Kind: sessiondata.PartMedia, State: model.ContentRemoved, Bytes: 4_628_610, SHA256: "ab"},
	}}
	var s step
	fill(&s, rec, nil)
	if s.State != model.ContentRemoved || s.Text != "" {
		t.Fatalf("the step says %q with text %q, want removed and no text", s.State, s.Text)
	}
	mixed := &sessiondata.Record{Parts: []sessiondata.Part{
		{Kind: sessiondata.PartText, State: model.ContentRemoved, Bytes: 12},
		{Kind: sessiondata.PartMedia, State: model.ContentTooLarge, Bytes: 4_628_610, SHA256: "ab"},
	}}
	var m step
	fill(&m, mixed, nil)
	if m.State != model.ContentRemoved {
		t.Fatalf("a record with a part too large and a part removed says %q, want removed", m.State)
	}
}

// A removed provider body file is served as its skeleton, read through its
// compression, byte for byte the Session Data the skeleton holds; a skeleton
// that is gone is left out, as a landed file that is gone is.
func TestARemovedBodyFileIsServedAsItsSkeleton(t *testing.T) {
	var landed bytes.Buffer
	w, err := sessiondata.NewWriter(&landed, &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Seq: 3,
		At: "2026-10-01T00:00:00Z", Kind: sessiondata.KindProviderBody, Adapter: "test/0", Dialect: "test/1", Src: ".", Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	m := json.RawMessage(`{"schema":"provider_body/1","role":"request","sha256":"` + strings.Repeat("a", 64) + `","bytes":2,"depth":0,"segments":[{"lit":"{}"}]}`)
	if err := w.Write(&sessiondata.Record{Ord: 1, ID: "r1", Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: m}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var sk bytes.Buffer
	if err := skeleton.Write(&sk, landed.Bytes(), sessiondata.Removed{Generation: 1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "provider_body-20261001T000000.000000000Z-000003.sk")
	if err := os.WriteFile(path, sk.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(sk.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	got, kind, err := readProviderBody(path)
	if err != nil || kind != sessiondata.KindProviderBody || !bytes.Equal(got, want) {
		t.Fatalf("the skeleton was served as %d bytes of %s, %v; want its %d bytes of Session Data", len(got), kind, err, len(want))
	}
	if _, _, err := readProviderBody(path + ".gone.sk"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a skeleton that is gone read as %v", err)
	}
}
