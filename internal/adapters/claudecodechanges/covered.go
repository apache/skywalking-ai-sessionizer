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
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
)

// Covered reports whether collecting s now would land nothing, because every
// change file of it is landed to its end. The results mean what they mean for
// the local adapter's Covered, and the rule is the same one, since both
// adapters tail their files with claudecode.TailAppend. Nothing is written,
// and the caller holds the session's lock.
func (c *Collector) Covered(s Session) (bool, string, error) {
	cursorOf := func(src Source) string {
		return filepath.Join(c.Zone.StreamDir(src.Session, src.Stream), prefix+".cursor")
	}
	// Checked for every file before any is read. Two plugin directories can
	// each hold a file for one stream. Both map to one cursor, and a pass
	// would interleave them behind it, so neither can be said to be landed to
	// its end, even while another file is still waiting to land.
	claimed := make(map[string]string, len(s.Sources))
	for _, src := range s.Sources {
		cursorPath := cursorOf(src)
		if prev, dup := claimed[cursorPath]; dup && prev != src.Rel {
			return false, "", fmt.Errorf("claudecodechanges: %s and %s both map to %s, so neither can be said to be landed", prev, src.Rel, cursorPath)
		}
		claimed[cursorPath] = src.Rel
	}
	for _, src := range s.Sources {
		ok, reason, err := claudecode.CoveredAppend(src.Path, src.Rel, cursorOf(src), c.MaxDelta)
		if err != nil || !ok {
			return ok, reason, err
		}
	}
	return true, "", nil
}
