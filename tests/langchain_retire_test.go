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
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// TestALangChainConversationIsRetiredByTheSameLevels. A LangChain
// conversation has no source to outlive, so asz alone gives it a lifetime,
// by the same two levels as every runtime. Once it is idle for retire_after,
// everything of it goes, and the runs remembered in open.json are kept as
// they were: an update carries nothing to find its conversation by. A later
// turn of its thread is not collected, and nothing of it is made again.
func TestALangChainConversationIsRetiredByTheSameLevels(t *testing.T) {
	zone, post, collect := receiveInto(t)
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-retired"}}`
	turn := func(id, question, at string) string {
		return `{"post":[{"id":"` + id + `","trace_id":"` + id + `",` +
			`"dotted_order":"20260920T10000` + at + `000000Z` + id + `",` +
			`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:0` + at + `Z",` +
			`"end_time":"2026-09-20T10:00:0` + at + `Z",` + owner + `,` +
			`"inputs":{"messages":[{"role":"user","content":"` + question + `"}]},` +
			`"outputs":{"messages":[{"role":"assistant","content":"ok"}]}}]}`
	}
	post(turn("52525252-5252-7252-8252-525252525251", "first question", "1"))
	session := collect().Sessions[0]
	for {
		round, err := parse.Session(zone, parse.Options{Conversation: session, Session: session})
		if err != nil {
			t.Fatal(err)
		}
		if !round.Changed() || !round.More {
			break
		}
	}
	pending := filepath.Join(zone.Root(), langsmith.InboxDir, "open.json")
	remembered, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}

	rem := &retention.Remover{Zone: zone, MetadataOnlyAfter: time.Hour, RetireAfter: time.Hour,
		Now: func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || len(res.Retired) != 1 || res.Retired[0] != session {
		t.Fatalf("the conversation was not retired: %+v", res)
	}
	if retired, err := storage.Retired(zone, session); err != nil || !retired {
		t.Fatalf("no marker: %v", err)
	}
	for _, dir := range []string{zone.SessionDir(session), filepath.Join(zone.Root(), "_conversations", session)} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s is left: %v", dir, err)
		}
	}
	if after, err := os.ReadFile(pending); err != nil || !bytes.Equal(after, remembered) {
		t.Fatalf("the retirement changed what open.json remembers: %v", err)
	}

	post(turn("52525252-5252-7252-8252-525252525252", "second question", "5"))
	landed := collect()
	if len(landed.Sessions) != 0 || len(landed.Retired) != 1 || landed.Retired[0] != session {
		t.Fatalf("the later turn of a retired conversation: %+v", landed)
	}
	if _, err := os.Stat(zone.SessionDir(session)); !os.IsNotExist(err) {
		t.Fatalf("the retired conversation's directory was made again: %v", err)
	}
}
