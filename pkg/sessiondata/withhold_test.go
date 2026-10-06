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

package sessiondata

import (
	"encoding/json"
	"testing"
)

// An adapter names a record by whether a field known to carry what a flag
// names holds a value, not by the shape the value has. Every shape a field
// could take is a value except the empty ones.
func TestHoldsValue(t *testing.T) {
	for raw, want := range map[string]bool{
		`["You are a helpful assistant."]`: true,
		`"You are a helpful assistant."`:   true,
		`{"Read":{}}`:                      true,
		`0`:                                true,
		`false`:                            true,
		`[ ]`:                              false,
		`{ }`:                              false,
		`""`:                               false,
		`null`:                             false,
		``:                                 false,
		`  null  `:                         false,
		`[`:                                true,
		`{`:                                true,
		`[1`:                               true,
	} {
		if got := HoldsValue(json.RawMessage(raw)); got != want {
			t.Errorf("HoldsValue(%q) = %v, want %v", raw, got, want)
		}
	}
}
