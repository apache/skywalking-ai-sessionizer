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

package langsmith

import (
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// The check that every record can be written writes them to nowhere, under
// the budget, and must not change them: a question it fitted would be gone
// when the conversation is placed and named afterwards.
func TestTheEncodableCheckChangesNoRecord(t *testing.T) {
	c := &Collector{MaxDeltaBytes: 4 << 10, Now: func() time.Time { return time.Unix(0, 0) }}
	question := strings.Repeat("why is the sky blue? ", 1_000)
	records := []sessiondata.Record{{ID: "r1", Bytes: len(question),
		Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: question, State: model.ContentAvailable}}}}
	if err := c.encodable("s1", "main", records, Waiting{Path: "request-1.json"}); err != nil {
		t.Fatal(err)
	}
	if p := records[0].Parts[0]; p.Text != question || p.State != model.ContentAvailable {
		t.Fatalf("the check left the question %s with %d bytes of text", p.State, len(p.Text))
	}
}
