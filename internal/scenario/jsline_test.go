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

package scenario

import (
	"bytes"
	"encoding/json"
	"testing"
)

// A literal backslash followed by u2028 is different from the character.
// Scenario sources must preserve both while matching the runtime's encoding.
func TestRuntimeJSONCharacters(t *testing.T) {
	value := "<>&\u2028\u2029" + `\u2028\u2029` + "\\\u2028"
	encoded := jsLine(map[string]any{"text": value})
	if !bytes.Contains(encoded, []byte("<>&\u2028\u2029")) {
		t.Fatalf("runtime characters were escaped: %q", encoded)
	}
	var decoded map[string]string
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["text"] != value {
		t.Fatalf("escaped text changed: %q", decoded["text"])
	}
}
