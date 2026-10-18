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

package tests_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	view_ "github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// TestACardStandsBeforeItsCallAndCarriesNothing reads the page a reader is
// served for a history trimmed with no mark, as landed and again after
// retention removed what the files held: the card is just before its call,
// under the same parent, names the two requests' records, and carries no
// text, state, bytes, flags or dropped parts, while its counts stay.
func TestACardStandsBeforeItsCallAndCarriesNothing(t *testing.T) {
	sc, err := scenario.Load("scenarios/history-forget.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := scenario.Build(sc, scenario.FormatSD, out, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(out)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Changed() || !r.More {
			break
		}
	}
	page := func() *sessionview.Conversation {
		c, err := view_.New(z, nil).Load(b.Session)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := c.Build()
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	var cardAttrs string
	look := func(label string, doc *sessionview.Conversation) {
		t.Helper()
		var card *sessionview.Node
		byID := map[string]*sessionview.Node{}
		siblings := map[string][]string{}
		var walk func(n *sessionview.Node)
		walk = func(n *sessionview.Node) {
			byID[n.ID] = n
			for i := range n.Children {
				siblings[n.ID] = append(siblings[n.ID], n.Children[i].ID)
				walk(&n.Children[i])
			}
			if n.Kind == model.KindHistoryChange {
				card = n
			}
		}
		for i := range doc.Talks {
			walk(&doc.Talks[i])
		}
		if card == nil {
			t.Fatalf("%s: no card on the page", label)
		}
		var a struct {
			From string `json:"from_call"`
			To   string `json:"to_call"`
		}
		_ = json.Unmarshal(card.Attrs, &a)
		call, from := byID[a.To], byID[a.From]
		if call == nil || from == nil {
			t.Fatalf("%s: the card names %s and %s, which the page does not hold", label, a.From, a.To)
		}
		if card.Parent != call.Parent || *card.Ref != *call.Ref {
			t.Errorf("%s: the card is under %s at %v, its call under %s at %v", label, card.Parent, card.Ref, call.Parent, call.Ref)
		}
		list := siblings[call.Parent]
		for i, id := range list {
			if id == card.ID && (i+1 >= len(list) || list[i+1] != call.ID) {
				t.Errorf("%s: siblings %v; the card is not just before its call", label, list)
			}
		}
		requestOf := func(n *sessionview.Node) string {
			for _, p := range n.ProviderBodies {
				if p.Role == "request" {
					return fmt.Sprint(p.Ref)
				}
			}
			return ""
		}
		if len(card.Refs) != 2 || fmt.Sprint(card.Refs[0]) != requestOf(from) || fmt.Sprint(card.Refs[1]) != requestOf(call) {
			t.Errorf("%s: the card names %v, the requests are %s and %s", label, card.Refs, requestOf(from), requestOf(call))
		}
		if card.Text != "" || card.State != "" || card.Bytes != 0 || len(card.Flags) != 0 || len(card.Dropped) != 0 {
			t.Errorf("%s: the card carries content: %q %q %d %v %v", label, card.Text, card.State, card.Bytes, card.Flags, card.Dropped)
		}
		if cardAttrs == "" {
			cardAttrs = string(card.Attrs)
		} else if string(card.Attrs) != cardAttrs {
			t.Errorf("%s: the card says %s, and said %s", label, card.Attrs, cardAttrs)
		}
	}
	look("as landed", page())
	res := (&retention.Remover{Zone: z, MetadataOnlyAfter: time.Hour, Now: func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }}).Pass(retention.Input{})
	if len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	files, err := storage.LandedFiles(z, b.Session)
	if err != nil {
		t.Fatal(err)
	}
	skeletons := 0
	for _, lf := range files {
		if lf.Skeleton {
			skeletons++
		}
	}
	if skeletons == 0 {
		t.Fatal("retention removed nothing; the test needs it to")
	}
	look("after retention", page())
}
