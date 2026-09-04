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

package sessionview_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// The YAML rendering is the JSON document with the same keys in the same
// order, and it reads back to the same values.
func TestYAMLIsTheSameDocument(t *testing.T) {
	doc := &sessionview.Conversation{
		Format: sessionview.Format, Version: sessionview.Version, Conversation: "c1", Sessions: []string{"c1"},
		Head: sessionview.Head{Round: 2, Digest: "abc"}, Parser: "v1", Policy: "v1+idle=10m0s",
		Summary: sessionview.Summary{Title: "a: title with punctuation", State: sessionview.StateVerified, Problems: []string{},
			Talks: 1, From: 1767225600000, To: 1767225630000, Kinds: map[string]int{"talk": 1}, RelationTypes: map[string]int{}, Quality: map[string]int{}},
		Rounds: []sessionview.Round{{Round: 1, Digest: "abc", FromSeq: 1, ThroughSeq: 2, InputDigest: "in", Verified: true}},
		Files:  []sessionview.File{}, Streams: []sessionview.Stream{}, Segments: []sessionview.Segment{},
		Talks: []sessionview.Node{{ID: "talk/main/s1-cycle", Kind: "talk", At: 1767225600000, Attrs: json.RawMessage(`{"runs":1,"trigger":"external"}`),
			Text: "two\nlines", Children: []sessionview.Node{{ID: "input/1/1", Kind: "message.external", At: 1767225600000, Text: "hello"}}}},
		Relations: []sessionview.Relation{}, Unresolved: []sessionview.Unresolved{},
	}
	out, err := sessionview.MarshalYAML(doc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.HasPrefix(text, "format: asz.view\nversion: \"1.0\"\nconversation: c1\n") {
		t.Fatalf("the document does not start with its format and version:\n%s", text[:120])
	}
	// Block mappings, plain scalars: an empty map is still {} and a string
	// that needs quoting is quoted, but nothing keeps JSON's flow style.
	if strings.Contains(text, "{\"") || !strings.Contains(text, "head:\n    round: 2\n    digest: abc\n") || !strings.Contains(text, "title: 'a: title with punctuation'") {
		t.Fatalf("the rendering is not block style with plain scalars:\n%s", text)
	}
	var fromYAML, fromJSON any
	if err := yaml.Unmarshal(out, &fromYAML); err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(doc)
	if err := json.Unmarshal(js, &fromJSON); err != nil {
		t.Fatal(err)
	}
	// YAML decodes integers as int and JSON as float64; compare through JSON.
	a, _ := json.Marshal(fromYAML)
	b, _ := json.Marshal(fromJSON)
	var va, vb any
	_ = json.Unmarshal(a, &va)
	_ = json.Unmarshal(b, &vb)
	if !reflect.DeepEqual(va, vb) {
		t.Fatalf("the YAML reads back differently:\n%s\n%s", a, b)
	}
}
