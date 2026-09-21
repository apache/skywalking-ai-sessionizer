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

package tests_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// TestWhatTheDocumentationClaims prints the numbers the adapter page states,
// read from the captures through the code that lands them.
//
// The page says every number in it came from the fixtures. That is only true
// if the numbers are produced rather than typed, because the landing rules
// change and a measurement copied by hand does not. Run it with -v and the
// page can be checked line by line:
//
//	go test ./tests/ -run TestWhatTheDocumentationClaims -v
func TestWhatTheDocumentationClaims(t *testing.T) {
	cases := []string{"plain", "three-turns", "tool-error", "parallel-tools",
		"loop", "subagent", "long-conversation", "large-content", "traceable-only"}
	var totalArrived, totalLanded int64
	for _, kase := range cases {
		arrived := requestBytes(t, kase)
		zone, sessions := land(t, kase)
		var landed, bodies int64
		for _, session := range sessions {
			files, err := storage.LandedFiles(zone, session)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				info, err := os.Stat(f.Path)
				if err != nil {
					t.Fatal(err)
				}
				// A provider body sits at the session, in no stream and no run.
				if f.Stream == "" && f.RunID == "" {
					bodies += info.Size()
					continue
				}
				landed += info.Size()
			}
		}
		totalArrived += arrived
		totalLanded += landed + bodies
		t.Logf("%-18s arrived %9d  conversation %8d (%5.1f%%)  bodies %8d (%5.1f%%)  together %5.1f%%",
			kase, arrived, landed, 100*float64(landed)/float64(arrived),
			bodies, 100*float64(bodies)/float64(arrived), 100*float64(landed+bodies)/float64(arrived))
	}
	t.Logf("%-18s arrived %9d  landed %8d  %5.1f%%",
		"ALL", totalArrived, totalLanded, 100*float64(totalLanded)/float64(totalArrived))
	// The numbers above are what the adapter page states. A measurement that
	// only prints would pass through a change that doubled what lands, so
	// the whole corpus is held to a bound well above what it measures today
	// (17.8%) and well below what landing bodies whole would give.
	if share := 100 * float64(totalLanded) / float64(totalArrived); share > 25 {
		t.Errorf("the corpus lands at %.1f%% of the wire; the page says 17.8%%, and above 25%% the trade the page describes is no longer the one being made", share)
	}
}

// requestBytes is what one case put on the wire.
func requestBytes(t *testing.T, kase string) int64 {
	t.Helper()
	dir := filepath.Join(corpus, kase)
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no corpus for %s: %v", kase, err)
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name())
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name, "body.bin"))
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}
