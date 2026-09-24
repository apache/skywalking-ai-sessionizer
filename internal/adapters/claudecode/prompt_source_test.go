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

package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
)

// TestAPromptSentThroughTheSDK reads the record shapes measured on the local
// corpus. Only a prompt with no origin and no isMeta is a caller's: the SDK
// also writes promptSource on notifications, which keep their origin, and on
// a harness notice, which is meta and is not something anyone asked.
func TestAPromptSentThroughTheSDK(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     string
		external bool
		trigger  index.Trigger
	}{
		{"headless prompt", `{"type":"user","promptSource":"sdk","message":{"role":"user","content":"read the README"}}`,
			true, index.TriggerExternal},
		{"notification through the SDK", `{"type":"user","promptSource":"sdk","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>"}}`,
			false, index.TriggerNotification},
		{"harness notice through the SDK", `{"type":"user","promptSource":"sdk","isMeta":true,"message":{"role":"user","content":"[Cross-session idle notice]"}}`,
			false, index.TriggerNone},
		{"prompt a person typed in VS Code", `{"type":"user","promptSource":"sdk","origin":{"kind":"human"},"message":{"role":"user","content":"hello"}}`,
			true, index.TriggerExternal},
		{"no promptSource and no origin", `{"type":"user","message":{"role":"user","content":"Continue from where you left off."}}`,
			false, index.TriggerNone},
	} {
		var d indexRecord
		if err := json.Unmarshal([]byte(tc.line), &d); err != nil {
			t.Fatal(err)
		}
		if got := flagsOf(&d, nil, false, Source{}).Has(index.FlagExternalInput); got != tc.external {
			t.Errorf("%s: external input %v, want %v", tc.name, got, tc.external)
		}
		if got := triggerOf(&d); got != tc.trigger {
			t.Errorf("%s: trigger %v, want %v", tc.name, got, tc.trigger)
		}
	}
}
