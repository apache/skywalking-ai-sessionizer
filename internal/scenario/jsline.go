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
	"strings"
	"unicode/utf8"
)

// jsLine encodes v the way Claude Code writes a line: JavaScript's
// JSON.stringify, which writes <, > and & as they are, and U+2028 and U+2029
// as the characters. Go's encoder writes those two as escapes inside a
// string whatever it is told, so each such escape is turned back into its
// character. Go wrote every byte here and writes a backslash as two, so a
// \u2028 it wrote always stands for the character.
func jsLine(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	src := bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		if src[i] != '\\' || i+1 == len(src) {
			out = append(out, src[i])
			continue
		}
		if src[i+1] == 'u' && i+6 <= len(src) {
			switch string(src[i+2 : i+6]) {
			case "2028":
				out = utf8.AppendRune(out, 0x2028)
				i += 5
				continue
			case "2029":
				out = utf8.AppendRune(out, 0x2029)
				i += 5
				continue
			}
		}
		// Any other escape is copied whole, so the second backslash of an
		// escaped backslash is never read as the start of an escape.
		out = append(out, src[i], src[i+1])
		i++
	}
	return out
}

// runName keeps workflow paths portable. A leading letter also makes names
// that start with punctuation valid for the adapter's run id pattern.
func runName(name string) string {
	result := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, name)
	if result == "" || result[0] == '-' || result[0] == '_' {
		result = "run-" + result
	}
	return result
}
