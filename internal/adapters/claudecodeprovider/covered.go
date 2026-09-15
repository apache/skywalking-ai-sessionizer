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
	"errors"
	"os"
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// SessionFiles lists the body files the adapter has attributed to a session,
// landed or not, that are still in the source directory. Nothing is written.
func (c *Collector) SessionFiles(session string) (map[string]bool, error) {
	sn, err := loadSeen(c.Zone)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for name, e := range sn.entries {
		if e.session != session {
			continue
		}
		if _, err := os.Lstat(filepath.Join(c.SourceRoot, name)); err == nil {
			out[name] = true
		}
	}
	return out, nil
}

// Covered reports whether the named body files of a session are landed in
// it, each with the bytes the file holds now. It reads the session's landed
// records, not the table, so a landed file lost since is not taken for
// landed. A removal deletes a body only then. Nothing is written.
func (c *Collector) Covered(session string, names []string) (bool, string, error) {
	held := map[string]string{}
	files, err := storage.LandedFiles(c.Zone, session)
	if err != nil {
		return false, "", err
	}
	for _, lf := range files {
		if lf.Stream != "" || lf.RunID != "" {
			continue
		}
		if err := readLanded(lf.Path, func(_ sessiondata.Header, rec *sessiondata.Record) {
			if m, err := providerbody.ManifestOf(rec); err == nil {
				held[m.Src] = m.SHA256
			}
		}); err != nil {
			return false, "", err
		}
	}
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(c.SourceRoot, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, name + " is missing", nil
		}
		if err != nil {
			return false, "", err
		}
		if held[name] != providerbody.Digest(body) {
			return false, "the provider body " + name + " is not landed with the bytes it holds", nil
		}
	}
	return true, "", nil
}
