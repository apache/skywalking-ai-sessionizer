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

package claudecodeprovider

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
)

// This file is the whole of what the adapter knows about how Claude Code names
// and fills its body files. Everything past it reads the model's words.

// ID is the record id of a body file: its name without the .json extension,
// such as "fcc261af-….request" or "req_011C….response". The role stays in
// the id, so a request and a response never share one.
func ID(file string) string { return strings.TrimSuffix(file, ".json") }

// RoleOf reports the role a file name gives a body, or "" for a name Claude
// Code does not write.
func RoleOf(file string) string {
	switch {
	case strings.HasSuffix(file, ".request.json"):
		return providerbody.RoleRequest
	case strings.HasSuffix(file, ".response.json"):
		return providerbody.RoleResponse
	}
	return ""
}

var (
	promptIDRe = regexp.MustCompile(`cc_prompt_id=([^;\s]+)`)
	prevReqRe  = regexp.MustCompile(`cc_prev_req=([^;\s]+)`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Lift reads the keys from a body Claude Code wrote under file, in the
// model's words. A value the body does not carry stays empty; nothing is
// inferred.
//
// For Claude Code 2.1.260: metadata.user_id is a JSON string holding
// session_id; the first system block is a billing header holding
// cc_prompt_id, the run, and cc_prev_req, the previous request; a response's
// id is the call; a response file is named by the provider's request id, or
// by a random UUID when Claude Code had none.
func Lift(file string, body []byte) providerbody.Keys {
	var k providerbody.Keys
	role := RoleOf(file)
	if role == providerbody.RoleResponse {
		// The name holds even when the body is not what a response should be.
		if stem := strings.TrimSuffix(file, ".response.json"); !uuidRe.MatchString(stem) {
			k.Request = stem
		}
	}
	var head map[string]json.RawMessage
	if json.Unmarshal(body, &head) != nil {
		return k
	}
	str := func(raw json.RawMessage) string {
		var v string
		_ = json.Unmarshal(raw, &v)
		return v
	}
	k.Model = str(head["model"])
	switch role {
	case providerbody.RoleRequest:
		var meta map[string]json.RawMessage
		_ = json.Unmarshal(head["metadata"], &meta)
		var user struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(str(meta["user_id"])), &user) == nil {
			k.Session = user.SessionID
		}
		header := firstSystemText(head["system"])
		if m := promptIDRe.FindStringSubmatch(header); m != nil {
			k.Run = m[1]
		}
		if m := prevReqRe.FindStringSubmatch(header); m != nil {
			k.PreviousRequest = m[1]
		}
	case providerbody.RoleResponse:
		k.Call = str(head["id"])
	}
	return k
}

// BodyOf is a body file as a session cuts it.
func BodyOf(file string, body []byte) providerbody.Body {
	return providerbody.Body{ID: ID(file), Role: RoleOf(file), Src: file, Keys: Lift(file, body), Bytes: body}
}

// firstSystemText is the text of the first system block, or the system
// prompt itself when it is one string.
func firstSystemText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
		return blocks[0].Text
	}
	return ""
}
