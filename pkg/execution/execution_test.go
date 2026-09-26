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

package execution_test

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
)

// The digest tells whether a value was changed on the way to the server, so
// one value has one digest however its keys and white space were written,
// and two values never share one.
func TestMeasureIsOneDigestPerValue(t *testing.T) {
	digest := func(s string) string { return execution.Measure([]byte(s)).SHA256 }
	if digest(`{"b": 1, "a": [true, "x"]}`) != digest(`{"a":[true,"x"],"b":1}`) {
		t.Error("one value written in two key orders has two digests")
	}
	// Two integers above 2^53 are one float64.
	if digest(`{"id":9007199254740992}`) == digest(`{"id":9007199254740993}`) {
		t.Error("two different integers have one digest")
	}
	if got := execution.Measure([]byte(`{"a":"<b>"}`)); got.Bytes != len(`{"a":"<b>"}`) {
		t.Errorf("%d bytes; the text must not be escaped", got.Bytes)
	}
	// Not one JSON value: measured as the bytes it is.
	for _, s := range []string{`not json`, `{} {}`} {
		if got := execution.Measure([]byte(s)); got.Bytes != len(s) {
			t.Errorf("%q: %d bytes, want the bytes as they are", s, got.Bytes)
		}
	}
}
