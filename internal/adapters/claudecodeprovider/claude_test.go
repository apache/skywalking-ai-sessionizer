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

package claudecodeprovider_test

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
)

func TestLiftReadsOnlyWhatTheBodyCarries(t *testing.T) {
	request := []byte(`{"model":"claude-opus-5","messages":[],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.260; cc_prev_req=req_prev; cc_prompt_id=bdee6995;"}],` +
		`"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"1a3cfda7-659c-474a-a448-cebc9370530f\"}"}}`)
	k := claudecodeprovider.Lift("a640339e-9d1c-4b6c-9a1e-2f9b6c1d0e11.request.json", request)
	want := providerbody.Keys{Model: "claude-opus-5", Session: "1a3cfda7-659c-474a-a448-cebc9370530f", Run: "bdee6995", PreviousRequest: "req_prev"}
	if k != want {
		t.Fatalf("request keys %+v, want %+v", k, want)
	}
	k = claudecodeprovider.Lift("req_011Cf4.response.json", []byte(`{"id":"msg_1","model":"claude-opus-5"}`))
	if k != (providerbody.Keys{Model: "claude-opus-5", Request: "req_011Cf4", Call: "msg_1"}) {
		t.Fatalf("response keys %+v", k)
	}
	// A response Claude Code had no request id for is named by a random UUID,
	// which is no provider request id.
	if k := claudecodeprovider.Lift("5d0c2d2a-0b8e-4f6a-9a55-3f1c1a0d9e21.response.json", []byte(`{"id":"msg_2"}`)); k.Request != "" {
		t.Fatalf("a UUID file name was taken for a request id: %+v", k)
	}
	// The name holds even when the body is not an object.
	if k := claudecodeprovider.Lift("req_x.response.json", []byte(`[]`)); k.Request != "req_x" {
		t.Fatalf("a response that is no object lost its request id: %+v", k)
	}
}
