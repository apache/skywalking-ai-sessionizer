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

package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// TalkViews gives each main talk's view digest: the SHA-256 of what a run is
// shown of the talk's work. That is every node of the main talk and of the
// sub-tasks it started, at any depth, with every field the navigation
// serves; the change and execution records joined to those nodes; and the
// starts relations that place the sub-tasks under it, each in document
// order. The document is the one built before judge.hide withholds
// anything, so a reader's setting never makes a talk look changed.
//
// asz server judges a talk again when its digest moves. The digest needs no
// rule about which evidence affects which talk: a run journal that attaches
// a child late, a provider body that changes the history a call node shows,
// or one that takes a join away all change what a run is shown, and so the
// digest.
func TalkViews(doc *sessionview.Conversation) map[string]string {
	l := Compute(doc, "", nil, nil)
	owner, ofStream := map[string]string{}, map[string]string{}
	for _, st := range l.Subtasks {
		if st.Main != "" {
			owner[st.Name], ofStream[st.ID] = st.Main, st.Main
		}
	}
	sums := map[string]hash.Hash{}
	write := func(talk, part string, v any) {
		h := sums[talk]
		if h == nil {
			h = sha256.New()
			sums[talk] = h
		}
		b, _ := json.Marshal(v)
		fmt.Fprintf(h, "%s %d\n", part, len(b))
		h.Write(b)
	}
	stepOwner := map[string]string{}
	for i := range doc.Talks {
		t := &doc.Talks[i]
		main := t.ID
		if t.Stream != "main" {
			main = owner[t.Stream]
		}
		if main == "" {
			continue
		}
		write(main, "talk", t)
		walkAll(t, func(n *sessionview.Node) { stepOwner[n.ID] = main })
	}
	for _, c := range doc.WorkspaceChanges {
		if main := stepOwner[c.Step]; main != "" {
			write(main, "change", c)
		}
	}
	for _, e := range doc.ToolExecutions {
		if main := stepOwner[e.Step]; main != "" {
			write(main, "execution", e)
		}
	}
	for _, r := range doc.Relations {
		if main := ofStream[r.To]; main != "" && r.Type == model.RelStarts {
			write(main, "starts", r)
		}
	}
	out := make(map[string]string, len(l.Main))
	for _, id := range l.Main {
		if h := sums[id]; h != nil {
			out[id] = hex.EncodeToString(h.Sum(nil))
		}
	}
	return out
}
