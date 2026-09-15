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

package main

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A session whose transcripts were pruned is judged by the directory its
// landed transcripts name, never collected because the root holds it.
func TestProviderFilterJudgesAPrunedSessionByItsLandedTranscripts(t *testing.T) {
	const session = "11111111-2222-4333-8444-555555555555"
	zone := storage.NewZone(t.TempDir())
	dir := zone.StreamDir(session, storage.StreamMain)
	err := storage.WriteAtomic(filepath.Join(dir, storage.LandedName("transcript", "20260101T000000.000000000Z", 1)), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, &sessiondata.Header{Seq: 1, Kind: sessiondata.KindTranscript, Adapter: "claude-code-local/0.1.0",
			Dialect: "claude-code/1", Src: "-private-tmp-scratch/" + session + ".jsonl", Session: session, Stream: storage.StreamMain})
		if err != nil {
			return err
		}
		return rw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	projects := t.TempDir() // discovery finds nothing: the transcript is pruned
	ad := config.Adapter{Exclude: []string{"/private/tmp/**"}}
	if v := providerFilter(ad, projects, zone)(session); v != claudecodeprovider.Exclude {
		t.Fatalf("a pruned session under an excluded directory: verdict %d, want Exclude", v)
	}
	if v := providerFilter(config.Adapter{}, projects, zone)(session); v != claudecodeprovider.Collect {
		t.Fatalf("a pruned session with no filter: verdict %d, want Collect", v)
	}
	if v := providerFilter(config.Adapter{}, projects, zone)("66666666-7777-4888-8999-aaaaaaaaaaaa"); v != claudecodeprovider.Wait {
		t.Fatalf("a session nothing names a directory for: verdict %d, want Wait", v)
	}
}
