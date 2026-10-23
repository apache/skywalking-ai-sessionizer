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

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
)

// Requested reads the file a call to one of Claude Code's editing tools
// names, and what it asks: Write writes a file whole, Edit and NotebookEdit
// change part of one. In the 60 most recent transcripts of one machine on
// 2026-10-23, every Edit (1,040) and Write (666) named its file in
// file_path; NotebookEdit names it in notebook_path, as measured on a run of
// Claude Code 2.1.260. No MultiEdit call was found, so none is read. A call
// to any other tool, or an input that names no file, gives nothing.
func Requested(tool string, input json.RawMessage) []changes.Requested {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	var path, op string
	switch tool {
	case "Write":
		path, op = in.FilePath, changes.RequestWrite
	case "Edit":
		path, op = in.FilePath, changes.RequestEdit
	case "NotebookEdit":
		path, op = in.NotebookPath, changes.RequestEdit
	}
	if path == "" {
		return nil
	}
	return []changes.Requested{{Path: path, Operation: op}}
}
