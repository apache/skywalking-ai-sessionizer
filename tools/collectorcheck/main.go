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
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// The shape the Collector's file exporter writes: one JSON line per
// request, in OTLP's JSON mapping.
type export struct {
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
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e export
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("a line the exporter wrote is not OTLP JSON: %w", err)
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
	for _, k := range []string{"transcript", "agent_meta", "journal", "workflow_manifest", "workflow_script", "round"} {
		if kinds[k] == 0 {
			return fmt.Errorf("no record of kind %s reached the Collector; it received %v", k, kinds)
		}
	}
	files := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(path, ".sd") || strings.HasSuffix(path, ".sf")) {
			files++
		}
		return nil
	})
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
		if _, err := sessionflow.OpenChain(twin, s).Verify(); err != nil {
			return fmt.Errorf("%s: the chain rebuilt from the Collector: %w", s, err)
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
	fmt.Printf("ok: %d requests, %d records, kinds %v, resources %v\n", requests, len(recs), kinds, services)
	return nil
}
