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
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
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

// A removed provider body keeps no bytes, so it is left out of what the
// files endpoint serves, as a file that is gone is.
func TestARemovedBodyIsNotServed(t *testing.T) {
	_, _, err := readProviderBody(filepath.Join(t.TempDir(), "provider_body-20261001T000000.000000000Z-000003.sk"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a removed body read as %v, want a file that is gone", err)
	}
}
