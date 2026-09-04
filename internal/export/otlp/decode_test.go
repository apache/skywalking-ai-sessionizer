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

package otlp_test

import (
	"reflect"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
)

// What Encode writes, Decode reads back unchanged.
func TestDecodeReadsWhatEncodeWrites(t *testing.T) {
	in := []otlp.ResourceLogs{{
		Resource:  []otlp.Attr{{Key: "service.name", Str: "Claude Code"}, {Key: "n", Int: 7, IsInt: true}},
		ScopeName: otlp.ScopeName, ScopeVersion: "test",
		Records: []otlp.Record{
			{TimeNano: 1, ObservedNano: 2, Severity: 9, SeverityText: "INFO", Body: "{\"h\":1}\n",
				Attrs: []otlp.Attr{{Key: "asz.format", Str: "sd"}, {Key: "asz.seq", Int: 3, IsInt: true}}},
			{TimeNano: 4, ObservedNano: 5, Severity: 9, SeverityText: "INFO", Body: "line\n"},
		},
	}}
	out, err := otlp.Decode(otlp.Encode(in))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip differs:\n in  %+v\n out %+v", in, out)
	}
}
