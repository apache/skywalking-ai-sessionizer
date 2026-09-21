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

package claudecodechanges

import (
	"fmt"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
)

// Covered reports whether collecting s now would land nothing, because every
// change file of it is landed to its end. The results mean what they mean for
// the local adapter's Covered, and the rule is the same one, since both
// adapters tail their files with claudecode.TailAppend. Nothing is written,
// and the caller holds the session's lock.
func (c *Collector) Covered(s Session) (bool, string, error) {
	// The cursor of a source is the one collection reads it behind: the
	// plain one for the first source of a stream, its own for any other, so
	// two plugin directories that each hold a file for one stream are
	// judged each against its own cursor, as they were landed. Reading the
	// plain cursor to tell them apart writes nothing.
	cursorOf := func(src Source) (string, error) {
		return cursorFor(c.Zone.StreamDir(src.Session, src.Stream), c.origin(), src)
	}
	// Checked for every file before any is read: two sources behind one
	// cursor would have been interleaved by a pass, and neither could be
	// said to be landed to its end.
	claimed := make(map[string]string, len(s.Sources))
	for _, src := range s.Sources {
		cursorPath, err := cursorOf(src)
		if err != nil {
			return false, "", err
		}
		if prev, dup := claimed[cursorPath]; dup && prev != src.Rel {
			return false, "", fmt.Errorf("claudecodechanges: %s and %s both map to %s, so neither can be said to be landed", prev, src.Rel, cursorPath)
		}
		claimed[cursorPath] = src.Rel
	}
	for _, src := range s.Sources {
		cursorPath, err := cursorOf(src)
		if err != nil {
			return false, "", err
		}
		ok, reason, err := claudecode.CoveredAppend(src.Path, src.Rel, cursorPath, c.MaxDelta)
		if err != nil || !ok {
			return ok, reason, err
		}
	}
	return true, "", nil
}
