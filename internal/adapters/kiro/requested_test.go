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

package kiro

import (
	"encoding/json"
	"fmt"
	"testing"
)

// An fs_write call names the file it writes whole; any other tool or input
// names none.
func TestAnFsWriteNamesTheFileItWrites(t *testing.T) {
	for _, tc := range []struct {
		tool, input, want string
	}{
		{"fs_write", `{"path":"/w/config.yaml","text":"a: 1"}`, "[{/w/config.yaml write}]"},
		{"fs_write", `{"text":"a: 1"}`, "[]"},
		{"fs_write", `{"path":`, "[]"},
		{"read_file", `{"path":"/w/config.yaml"}`, "[]"},
		{"execute_bash", `{"command":"rm x"}`, "[]"},
	} {
		if got := fmt.Sprint(Requested(tc.tool, json.RawMessage(tc.input))); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.tool, tc.input, got, tc.want)
		}
	}
}
