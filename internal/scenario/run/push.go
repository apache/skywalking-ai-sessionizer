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

package run

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/mock"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/expect"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// receiver is an OTLP/HTTP logs endpoint that keeps every request, and can
// be told to refuse them.
type receiver struct {
	mu   sync.Mutex
	reqs [][]byte
	fail bool
}

func (r *receiver) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/logs" || req.Header.Get("Content-Type") != "application/x-protobuf" {
			http.Error(w, "wrong path or content type", http.StatusBadRequest)
			return
		}
		var body bytes.Buffer
		_, _ = body.ReadFrom(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		r.reqs = append(r.reqs, body.Bytes())
		w.WriteHeader(http.StatusOK)
	})
}

// The file kinds the export page names.
var wireKinds = map[string]bool{
	"transcript": true, "agent_meta": true, "journal": true, "workflow_manifest": true, "workflow_script": true, "round": true,
}

// pushFollowsTheWire pushes the finished session to a receiver and checks
// every request against the export page: the resource, one record per file
// with the file's bytes and digest, the attributes each format carries and
// the ones it must not, the stamp a receiver bounds a read on, delivery
// once and at least once, and the export path: writing every body back
// gives a root that verifies and folds the same.
func pushFollowsTheWire(out, session string, f scenario.Format, want *expect.Push) ([]string, error) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	_ = os.Remove(filepath.Join(out, "push.state"))
	newPusher := func() *otlp.Pusher {
		return &otlp.Pusher{
			Zone: storage.NewZone(out), Client: &otlp.Client{Endpoint: srv.URL}, Version: "check",
			ServiceName: "Scenario Check", InstanceID: "scenario-check", Layer: "AI_AGENT",
			// One byte: every file is larger than the budget, so every file
			// travels alone, in its own request.
			BatchBytes: 1,
			Now:        func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
		}
	}
	var out2 []string
	bad := func(format string, a ...any) {
		out2 = append(out2, "push_follows_the_wire: "+fmt.Sprintf(format, a...))
	}

	// With no service configured, a session is attributed to the runtime
	// that produced it, read off its landed header: the adapter's runtime
	// for a runtime format, the mock's for sd.
	named := newPusher()
	named.ServiceName = ""
	named.Runtimes = map[string]string{claudecode.Name: claudecode.RuntimeName, mock.Name: mock.RuntimeName}
	if _, err := named.Pass(); err != nil {
		return nil, err
	}
	wantService := mock.RuntimeName
	if f == scenario.FormatClaudeCode {
		wantService = claudecode.RuntimeName
	}
	for i, req := range rcv.reqs {
		groups, err := otlp.Decode(req)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if got := attrMap(g.Resource)["service.name"]; got != wantService {
				bad("request %d without a configured service names %q, want the runtime %q", i, got, wantService)
			}
		}
	}
	rcv.mu.Lock()
	rcv.reqs = nil
	rcv.mu.Unlock()
	_ = os.Remove(filepath.Join(out, "push.state"))

	// A refused pass leaves everything for the next one.
	rcv.fail = true
	st, err := newPusher().Pass()
	if err != nil {
		return nil, err
	}
	if st.Files != 0 || len(st.Errors) == 0 {
		bad("a refused request marked %d files pushed with %d errors; it must mark none", st.Files, len(st.Errors))
	}
	rcv.mu.Lock()
	rcv.fail = false
	rcv.mu.Unlock()

	files, err := landedAndRounds(out, session)
	if err != nil {
		return nil, err
	}
	st, err = newPusher().Pass()
	if err != nil {
		return nil, err
	}
	if len(st.Errors) != 0 {
		return nil, fmt.Errorf("push: %v", st.Errors)
	}
	if st.Files != len(files) || st.Requests != len(files) {
		bad("pushed %d files in %d requests; the session has %d files and a one-byte budget sends each alone", st.Files, st.Requests, len(files))
	}

	// Every request: the resource and the scope, as the page lists them.
	var records []otlp.Record
	for i, req := range rcv.reqs {
		groups, err := otlp.Decode(req)
		if err != nil {
			return nil, err
		}
		if len(groups) != 1 {
			bad("request %d carries %d resources, want 1", i, len(groups))
			continue
		}
		g := groups[0]
		res := attrMap(g.Resource)
		for k, v := range map[string]string{
			"service.name": "Scenario Check", "service.instance.id": "scenario-check", "service.layer": "AI_AGENT",
			"telemetry.sdk.name": "asz", "telemetry.sdk.version": "check", "telemetry.sdk.language": "go",
		} {
			if res[k] != v {
				bad("request %d resource %s is %q, want %q", i, k, res[k], v)
			}
		}
		if g.ScopeName != otlp.ScopeName || g.ScopeVersion != "check" {
			bad("request %d scope is %s %s", i, g.ScopeName, g.ScopeVersion)
		}
		records = append(records, g.Records...)
	}

	// Every record: one file, its bytes, its digest, and the attributes the
	// page says a landed file or a round carries, and no others.
	seen := map[string]bool{}
	kinds := map[string]bool{}
	latest := sessionLatest(files)
	for _, r := range records {
		a := attrMap(r.Attrs)
		f, ok := files[a["asz.file"]]
		if !ok {
			bad("record names %q, which is not a file of the session", a["asz.file"])
			continue
		}
		seen[a["asz.file"]] = true
		kinds[a["asz.file.kind"]] = true
		if r.Body != string(f.data) {
			bad("%s: the body is not the file's bytes", f.rel)
		}
		if a["asz.file.digest"] != f.digest {
			bad("%s: digest %s, the file's is %s", f.rel, a["asz.file.digest"], f.digest)
		}
		if a["asz.lines"] != fmt.Sprintf("int:%d", bytes.Count(f.data, []byte("\n"))) {
			bad("%s: lines %s, the file has %d", f.rel, a["asz.lines"], bytes.Count(f.data, []byte("\n")))
		}
		if a["asz.format"] != f.format || a["asz.format.version"] != f.schema || a["asz.file.kind"] != f.kind || !wireKinds[a["asz.file.kind"]] {
			bad("%s: format %s %s kind %s; the file is %s %s %s", f.rel, a["asz.format"], a["asz.format.version"], a["asz.file.kind"], f.format, f.schema, f.kind)
		}
		if a["asz.session"] != session {
			bad("%s: session %q", f.rel, a["asz.session"])
		}
		if r.Severity != 9 || r.SeverityText != "INFO" || r.ObservedNano == 0 {
			bad("%s: severity %d %q observed %d", f.rel, r.Severity, r.SeverityText, r.ObservedNano)
		}
		timed, from, through := f.from != "", f.from, f.through
		if (a["asz.from_time"] != "") != timed || a["asz.from_time"] != from || a["asz.through_time"] != through {
			bad("%s: time range %s..%s, the file's records say %s..%s", f.rel, a["asz.from_time"], a["asz.through_time"], from, through)
		}
		switch f.format {
		case "sd":
			if a["asz.seq"] != fmt.Sprintf("int:%d", f.seq) {
				bad("%s: seq %s, the header says %d", f.rel, a["asz.seq"], f.seq)
			}
			if (a["asz.stream"] != "") == (a["asz.run"] != "") || a["asz.stream"] != f.stream || a["asz.run"] != f.batch {
				bad("%s: stream %q run %q, the header says stream %q batch %q", f.rel, a["asz.stream"], a["asz.run"], f.stream, f.batch)
			}
			for _, k := range []string{"asz.conversation", "asz.round", "asz.session.from_time", "asz.session.through_time", "asz.conversation.title", "asz.conversation.talks"} {
				if _, has := a[k]; has {
					bad("%s: a landed file carries %s", f.rel, k)
				}
			}
			// The stamp: the file's last record time, or the session's
			// latest when the file's records carry none.
			want := through
			if !timed {
				want = latest
			}
			if want != "" && r.TimeNano != uint64(stampNS(want)) {
				bad("%s: stamped %d, want %s", f.rel, r.TimeNano, want)
			}
		case "sf":
			if a["asz.conversation"] != session || a["asz.round"] != fmt.Sprintf("int:%d", f.round) {
				bad("%s: conversation %q round %s, the header says round %d", f.rel, a["asz.conversation"], a["asz.round"], f.round)
			}
			for _, k := range []string{"asz.seq", "asz.stream", "asz.run"} {
				if _, has := a[k]; has {
					bad("%s: a round carries %s", f.rel, k)
				}
			}
			h := f.header
			if a["asz.session.from_time"] != h.SessionFromTime || a["asz.session.through_time"] != h.SessionThroughTime {
				bad("%s: session range %s..%s, the header says %s..%s", f.rel, a["asz.session.from_time"], a["asz.session.through_time"], h.SessionFromTime, h.SessionThroughTime)
			}
			if a["asz.conversation.title"] != h.Title || a["asz.conversation.talks"] != fmt.Sprintf("int:%d", h.Talks) ||
				a["asz.conversation.steps"] != fmt.Sprintf("int:%d", h.Steps) || a["asz.conversation.streams"] != fmt.Sprintf("int:%d", h.Streams) ||
				a["asz.conversation.segments"] != fmt.Sprintf("int:%d", h.Segments) || a["asz.conversation.unresolved"] != fmt.Sprintf("int:%d", h.Unresolved) {
				bad("%s: list attributes %v, the header says %q %d %d %d %d %d", f.rel, listAttrs(a), h.Title, h.Talks, h.Steps, h.Streams, h.Segments, h.Unresolved)
			}
			if r.TimeNano != uint64(stampNS(h.SessionThroughTime)) {
				bad("%s: stamped %d, want the session's last activity %s", f.rel, r.TimeNano, h.SessionThroughTime)
			}
		}
	}
	for rel := range files {
		if !seen[rel] {
			bad("%s was never sent", rel)
		}
	}
	if want != nil {
		for _, k := range want.Kinds {
			if !kinds[k] {
				bad("no record of kind %s was sent; the session has %v", k, keys(kinds))
			}
		}
	}

	// Sent once: a second pass sends nothing.
	before := len(rcv.reqs)
	st, err = newPusher().Pass()
	if err != nil {
		return nil, err
	}
	if st.Files != 0 || len(rcv.reqs) != before {
		bad("a second pass sent %d files again", st.Files)
	}

	// The export path: every body written to its path gives a root that
	// verifies and folds the same.
	twin := out + "-wire"
	defer os.RemoveAll(twin)
	for _, r := range records {
		a := attrMap(r.Attrs)
		path := filepath.Join(twin, filepath.FromSlash(a["asz.file"]))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(r.Body), 0o444); err != nil {
			return nil, err
		}
	}
	rep, err := verify.Session(storage.NewZone(twin), session)
	if err != nil {
		bad("the root rebuilt from the wire does not verify: %v", err)
	} else if !rep.OK() {
		bad("the root rebuilt from the wire has %d problem(s): %v", rep.Problems, rep.Details())
	}
	if _, err := sessionflow.OpenChain(twin, session).Verify(); err != nil {
		bad("the chain rebuilt from the wire does not verify: %v", err)
	}
	a, err := expect.Summarize(out, session)
	if err != nil {
		return nil, err
	}
	b, err := expect.Summarize(twin, session)
	if err != nil {
		bad("the root rebuilt from the wire does not fold: %v", err)
		return out2, nil
	}
	for _, d := range expect.Compare(a, b) {
		bad("the fold of the root rebuilt from the wire differs: %s", d)
	}
	return out2, nil
}

// wireFile is one file of the session as the receiver must see it.
type wireFile struct {
	rel, format, schema, kind, stream, batch string
	seq, round                               uint64
	data                                     []byte
	digest, from, through                    string
	header                                   sessionflow.Header
}

// landedAndRounds reads every file the push must send, keyed by its path
// on the wire, with what its header and records say.
func landedAndRounds(out, session string) (map[string]*wireFile, error) {
	files := map[string]*wireFile{}
	add := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, path)
		sum := sha256.Sum256(data)
		f := &wireFile{rel: filepath.ToSlash(rel), data: data, digest: hex.EncodeToString(sum[:])}
		first, _, _ := bytes.Cut(data, []byte("\n"))
		if strings.HasSuffix(path, ".sd") {
			var h sessiondata.Header
			if err := json.Unmarshal(first, &h); err != nil {
				return err
			}
			f.format, f.schema, f.kind, f.stream, f.batch, f.seq = "sd", h.Schema, string(h.Kind), h.Stream, h.Batch, h.Seq
		} else {
			if err := json.Unmarshal(first, &f.header); err != nil {
				return err
			}
			f.format, f.schema, f.kind, f.round = "sf", f.header.Schema, "round", f.header.Round
			f.from, f.through = f.header.FromTime, f.header.ThroughTime
		}
		if f.format == "sd" {
			var lo, hi int64
			ok := false
			for _, line := range bytes.Split(data, []byte("\n")) {
				if ns, has := sessiondata.LineTime(line); has {
					if !ok || ns < lo {
						lo = ns
					}
					if !ok || ns > hi {
						hi = ns
					}
					ok = true
				}
			}
			if ok {
				f.from, f.through = sessiondata.FormatTime(lo), sessiondata.FormatTime(hi)
			}
		}
		files[f.rel] = f
		return nil
	}
	landed, err := storage.LandedFiles(storage.NewZone(out), session)
	if err != nil {
		return nil, err
	}
	for _, lf := range landed {
		if err := add(lf.Path); err != nil {
			return nil, err
		}
	}
	rounds, err := sessionflow.OpenChain(out, session).List()
	if err != nil {
		return nil, err
	}
	for _, rf := range rounds {
		if err := add(rf.Path); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// sessionLatest is the latest record time among the session's landed
// files, the stamp a file without timed records gets.
func sessionLatest(files map[string]*wireFile) string {
	latest := ""
	for _, f := range files {
		if f.format == "sd" && f.through > latest {
			latest = f.through
		}
	}
	return latest
}

func attrMap(attrs []otlp.Attr) map[string]string {
	m := map[string]string{}
	for _, a := range attrs {
		if a.IsInt {
			m[a.Key] = fmt.Sprintf("int:%d", a.Int)
		} else {
			m[a.Key] = a.Str
		}
	}
	return m
}

func listAttrs(a map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		if strings.HasPrefix(k, "asz.conversation.") {
			out[k] = v
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stampNS(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixNano()
}
