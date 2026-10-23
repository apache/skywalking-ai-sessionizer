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

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
)

// Requested reads the file a call to Kiro's fs_write names. In the 4 session
// files of one machine that held fs_write calls on 2026-10-23, all 47 calls
// had the arguments path and text, the file written whole. A call to any
// other tool, or arguments that name no file, gives nothing.
func Requested(tool string, input json.RawMessage) []changes.Requested {
	if tool != "fs_write" {
		return nil
	}
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(input, &in) != nil || in.Path == "" {
		return nil
	}
	return []changes.Requested{{Path: in.Path, Operation: changes.RequestWrite}}
}
