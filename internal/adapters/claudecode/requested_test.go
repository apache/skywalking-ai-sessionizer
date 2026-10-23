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
	"fmt"
	"testing"
)

// An editing tool's input names the file it asks to change, in the keys
// Claude Code writes; any other tool or input names none.
func TestARequestNamesTheFileItEdits(t *testing.T) {
	for _, tc := range []struct {
		tool, input, want string
	}{
		{"Write", `{"file_path":"/w/a.go","content":"package a"}`, "[{/w/a.go write}]"},
		{"Edit", `{"file_path":"/w/a.go","old_string":"a","new_string":"b","replace_all":false}`, "[{/w/a.go edit}]"},
		{"Edit", `{"file_path":"/w/a.go","old_string":"a","new_string":"b"}`, "[{/w/a.go edit}]"},
		{"NotebookEdit", `{"notebook_path":"/w/n.ipynb","new_source":"x"}`, "[{/w/n.ipynb edit}]"},
		{"NotebookEdit", `{"file_path":"/w/n.ipynb"}`, "[]"},
		{"MultiEdit", `{"file_path":"/w/a.go","edits":[]}`, "[]"},
		{"Read", `{"file_path":"/w/a.go"}`, "[]"},
		{"Bash", `{"command":"rm a.go"}`, "[]"},
		{"Write", `{"content":"x"}`, "[]"},
		{"Write", `{"file_path":`, "[]"},
	} {
		if got := fmt.Sprint(Requested(tc.tool, json.RawMessage(tc.input))); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.tool, tc.input, got, tc.want)
		}
	}
}
