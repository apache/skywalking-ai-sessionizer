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

// Command collectorcheck verifies what a real OpenTelemetry Collector wrote
// with its file exporter against the storage root that was pushed to it:
// one record per file, every body the file's bytes, every digest matching,
// every kind present, and a root rebuilt from the bodies that verifies and
// folds the same. It is what the Collector job in CI runs after asz push.
//
//	go run ./tools/collectorcheck ROOT LOGS.JSON
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// The shape the Collector's file exporter writes: one JSON line per
// request, in OTLP's JSON mapping; a logs request or a metrics request.
type export struct {
	ResourceMetrics []struct {
		Resource struct {
			Attributes []attr `json:"attributes"`
		} `json:"resource"`
		ScopeMetrics []struct {
			Metrics []struct {
				Name string `json:"name"`
				Unit string `json:"unit"`
				Sum  struct {
					AggregationTemporality any  `json:"aggregationTemporality"`
					IsMonotonic            bool `json:"isMonotonic"`
					DataPoints             []struct {
						AsInt      string `json:"asInt"`
						Attributes []attr `json:"attributes"`
					} `json:"dataPoints"`
				} `json:"sum"`
			} `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
	ResourceLogs []struct {
		Resource struct {
			Attributes []attr `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			Scope struct {
				Name string `json:"name"`
			} `json:"scope"`
			LogRecords []struct {
				TimeUnixNano string `json:"timeUnixNano"`
				Body         struct {
					StringValue string `json:"stringValue"`
				} `json:"body"`
				Attributes []attr `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type attr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
		IntValue    string `json:"intValue"`
	} `json:"value"`
}

func attrs(as []attr) map[string]string {
	m := map[string]string{}
	for _, a := range as {
		if a.Value.IntValue != "" {
			m[a.Key] = a.Value.IntValue
		} else {
			m[a.Key] = a.Value.StringValue
		}
	}
	return m
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: collectorcheck ROOT LOGS.JSON")
		os.Exit(2)
	}
	root, logs := os.Args[1], os.Args[2]
	if err := run(root, logs); err != nil {
		fmt.Fprintln(os.Stderr, "collectorcheck:", err)
		os.Exit(1)
	}
}

func run(root, logs string) error {
	raw, err := os.ReadFile(logs)
	if err != nil {
		return err
	}
	// The exporter may leave a partial last line while it is still writing;
	// stray NULs come from that.
	raw = bytes.ReplaceAll(raw, []byte{0}, nil)

	type rec struct {
		file, digest, kind, session, format string
		lines                               int
		body                                string
	}
	var recs []rec
	services := map[string]int{}
	requests := 0
	// The token metric, as the Collector received it: points by query
	// source and type, and how many requests carried it.
	tokens := map[string]int64{}
	metricRequests := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e export
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("a line the exporter wrote is not OTLP JSON: %w", err)
		}
		if len(e.ResourceMetrics) > 0 {
			metricRequests++
			for _, rm := range e.ResourceMetrics {
				res := attrs(rm.Resource.Attributes)
				if res["service.name"] == "" || res["service.layer"] == "" || res["telemetry.sdk.name"] != "asz" {
					return fmt.Errorf("a metrics request carries the resource %v; asz's identity is missing", res)
				}
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						if m.Name != "claude_code.token.usage" || m.Unit != "tokens" || !m.Sum.IsMonotonic {
							return fmt.Errorf("a metrics request carries %s %s monotonic=%v", m.Name, m.Unit, m.Sum.IsMonotonic)
						}
						for _, dp := range m.Sum.DataPoints {
							a := attrs(dp.Attributes)
							n, _ := strconv.ParseInt(dp.AsInt, 10, 64)
							tokens[a["query_source"]+"/"+a["type"]] += n
						}
					}
				}
			}
			continue
		}
		requests++
		for _, rl := range e.ResourceLogs {
			res := attrs(rl.Resource.Attributes)
			services[res["service.name"]+"/"+res["service.layer"]+"/"+res["telemetry.sdk.name"]]++
			for _, sl := range rl.ScopeLogs {
				if sl.Scope.Name != "github.com/apache/skywalking-ai-sessionizer" {
					return fmt.Errorf("scope %q", sl.Scope.Name)
				}
				for _, lr := range sl.LogRecords {
					a := attrs(lr.Attributes)
					n, _ := strconv.Atoi(a["asz.lines"])
					recs = append(recs, rec{file: a["asz.file"], digest: a["asz.file.digest"], kind: a["asz.file.kind"],
						session: a["asz.session"], format: a["asz.format"], lines: n, body: lr.Body.StringValue})
				}
			}
		}
	}
	if len(recs) == 0 {
		return fmt.Errorf("the Collector wrote no records")
	}

	// Every record is one file of the root, byte for byte.
	kinds := map[string]int{}
	sessions := map[string]bool{}
	for _, r := range recs {
		want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(r.file)))
		if err != nil {
			return fmt.Errorf("%s: not a file of the root: %w", r.file, err)
		}
		if r.body != string(want) {
			return fmt.Errorf("%s: the body is not the file's bytes", r.file)
		}
		sum := sha256.Sum256([]byte(r.body))
		if hex.EncodeToString(sum[:]) != r.digest {
			return fmt.Errorf("%s: digest %s does not match the body", r.file, r.digest)
		}
		if strings.Count(r.body, "\n") != r.lines {
			return fmt.Errorf("%s: %d lines, the record says %d", r.file, strings.Count(r.body, "\n"), r.lines)
		}
		kinds[r.kind]++
		sessions[r.session] = true
	}
	// Every file of the root reached the Collector, and every kind the root
	// holds is among the records. The all-kinds scenario is built into the
	// root, so that is every kind the export page names.
	files := 0
	rootKinds := map[string]int{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".sf"):
			files++
			rootKinds["round"]++
		case strings.HasSuffix(path, ".sd"):
			files++
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			first, _, _ := bytes.Cut(data, []byte("\n"))
			var h struct {
				Kind string `json:"kind"`
			}
			if json.Unmarshal(first, &h) == nil {
				rootKinds[h.Kind]++
			}
		}
		return nil
	})
	for k, n := range rootKinds {
		if kinds[k] != n {
			return fmt.Errorf("the root holds %d files of kind %s, the Collector received %d; it received %v", n, k, kinds[k], kinds)
		}
	}
	for _, k := range []string{"transcript", "agent_meta", "journal", "workflow_manifest", "workflow_script", "round"} {
		if rootKinds[k] == 0 {
			return fmt.Errorf("the root holds no file of kind %s, so the check exercised less than the export page names", k)
		}
	}
	if len(recs) != files {
		return fmt.Errorf("the Collector received %d records, the root holds %d files", len(recs), files)
	}

	// The export path: the bodies written back are a root that verifies and
	// folds like the one that was pushed.
	twin, err := os.MkdirTemp("", "collectorcheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(twin)
	for _, r := range recs {
		path := filepath.Join(twin, filepath.FromSlash(r.file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(r.body), 0o444); err != nil {
			return err
		}
	}
	var ids []string
	for s := range sessions {
		ids = append(ids, s)
	}
	sort.Strings(ids)
	for _, s := range ids {
		rep, err := verify.Session(storage.NewZone(twin), s)
		if err != nil {
			return fmt.Errorf("%s rebuilt from the Collector: %w", s, err)
		}
		if !rep.OK() {
			return fmt.Errorf("%s rebuilt from the Collector has %d problem(s): %v", s, rep.Problems, rep.Details())
		}
		chain, err := verify.Chain(storage.NewZone(twin), s, nil)
		if err != nil {
			return fmt.Errorf("%s: the chain rebuilt from the Collector: %w", s, err)
		}
		if !chain.OK() {
			return fmt.Errorf("%s: the chain rebuilt from the Collector has %d problem(s): %v", s, chain.Problems, chain.Details())
		}
		a, err := parse.View(root, s)
		if err != nil {
			return err
		}
		b, err := parse.View(twin, s)
		if err != nil {
			return fmt.Errorf("%s: the root rebuilt from the Collector does not fold: %w", s, err)
		}
		if a.Digest != b.Digest || len(a.Nodes) != len(b.Nodes) || len(a.Relations) != len(b.Relations) {
			return fmt.Errorf("%s: the fold rebuilt from the Collector differs: %s/%d/%d against %s/%d/%d",
				s, a.Digest[:12], len(a.Nodes), len(a.Relations), b.Digest[:12], len(b.Nodes), len(b.Relations))
		}
		talks := len(b.NodesByKind(model.KindTalk))
		fmt.Printf("%s: %d talks, %d nodes, round %d, rebuilt from the Collector and verified\n", s, talks, len(b.Nodes), b.Round)
	}
	// The metrics the local adapter derived reached the Collector too:
	// every planned session has calls with usage, so tokens must arrive
	// under both query sources and all four types.
	for _, k := range []string{"main/input", "main/output", "main/cacheRead", "main/cacheCreation", "subagent/input", "subagent/output"} {
		if tokens[k] <= 0 {
			return fmt.Errorf("no %s tokens reached the Collector; it received %v", k, tokens)
		}
	}
	fmt.Printf("ok: %d requests, %d records, kinds %v, resources %v; %d metrics requests, tokens %v\n", requests, len(recs), kinds, services, metricRequests, tokens)
	return nil
}
