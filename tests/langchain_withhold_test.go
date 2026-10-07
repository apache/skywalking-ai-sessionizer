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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	view_ "github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// TestAnUnfinishedLangChainCallIsWithheld replays a capture as a client that
// puts a call's request inside the run's envelope would send it. A model call
// that arrives before it has finished lands that arrival's envelope whole, so
// its record carries the request, is named for it, and is withheld like any
// snapshot: the document counts it, and the record endpoint keeps its content
// back. The captured client sends the request out of band, which is why the
// capture is rewritten.
func TestAnUnfinishedLangChainCallIsWithheld(t *testing.T) {
	kase := unfinishedCase(t)
	if kase == "" {
		t.Skip("no capture holds a model call that arrived before it finished")
	}
	zone := storage.NewZone(t.TempDir())
	inlined := landInlined(t, zone, kase)
	sessions, err := os.ReadDir(zone.Root())
	if err != nil {
		t.Fatal(err)
	}
	var session string
	for _, d := range sessions {
		if d.IsDir() && !strings.HasPrefix(d.Name(), "_") {
			session = d.Name()
		}
	}
	if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := view_.New(zone, nil).Load(session)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.BuildWithheld([]string{sessiondata.FlagSystemPrompt})
	if err != nil {
		t.Fatal(err)
	}
	// The call and the error step its failure became stand on one record,
	// and one record withheld counts once.
	if got := doc.Summary.Withheld[sessiondata.FlagSystemPrompt]; got != inlined {
		t.Fatalf("withheld %v; want the %d unfinished call(s) counted under system_prompt", doc.Summary.Withheld, inlined)
	}
	var call *sessionview.Node
	var find func([]sessionview.Node)
	find = func(nodes []sessionview.Node) {
		for i := range nodes {
			for _, f := range nodes[i].Flags {
				if f == sessiondata.FlagSystemPrompt && nodes[i].Kind == "llm.call" {
					call = &nodes[i]
				}
			}
			find(nodes[i].Children)
		}
	}
	find(doc.Talks)
	find(doc.Loose)
	if call == nil || call.Kind != "llm.call" || call.State != "omitted" || call.Ref == nil {
		t.Fatalf("the unfinished call in the withheld document: %+v", call)
	}
	// Nothing read from the withheld record appears anywhere in the
	// document. The whole document shows the failure as the steps' text, so
	// the check is not empty.
	whole, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	shows := func(d *sessionview.Conversation) bool {
		raw, _ := json.Marshal(d)
		return strings.Contains(string(raw), callFailure)
	}
	if !shows(whole) {
		t.Fatal("the whole document does not show the failure, so this test checks nothing")
	}
	if shows(doc) {
		t.Fatal("the withheld document shows text read from the withheld record")
	}
	h := view_.New(zone, nil).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		return r
	}
	address := fmt.Sprintf("/api/c/%s/record/%d/%d", session, call.Ref.Seq, call.Ref.Row)
	if r := get(address); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "inputs") {
		t.Fatalf("the whole record does not carry the request: %d %.300s", r.Code, r.Body.String())
	}
	var rec sessiondata.Record
	r := get(address + "?hide=system_prompt")
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &rec) != nil {
		t.Fatalf("the withheld record: %d %.300s", r.Code, r.Body.String())
	}
	for _, p := range rec.Parts {
		if len(p.Data) != 0 || p.Text != "" || p.State != "omitted" {
			t.Fatalf("the withheld record kept content: %+v", p)
		}
	}
}

// unfinishedCase names the first capture holding a model call that arrived
// before it finished, found by rewriting each request without sending it.
func unfinishedCase(t *testing.T) string {
	t.Helper()
	cases, err := os.ReadDir(corpus)
	if err != nil {
		t.Skipf("no corpus at %s: %v", corpus, err)
	}
	for _, c := range cases {
		requests, _ := os.ReadDir(filepath.Join(corpus, c.Name()))
		for _, r := range requests {
			raw, err := os.ReadFile(filepath.Join(corpus, c.Name(), r.Name(), "meta.json"))
			if err != nil {
				continue
			}
			var meta struct {
				Headers map[string]string `json:"headers"`
			}
			body, berr := os.ReadFile(filepath.Join(corpus, c.Name(), r.Name(), "body.bin"))
			if json.Unmarshal(raw, &meta) != nil || berr != nil {
				continue
			}
			if _, _, n := inlineUnfinished(t, body, meta.Headers["Content-Type"]); n > 0 {
				return c.Name()
			}
		}
	}
	return ""
}

// callFailure is the failure written on a rewritten call.
const callFailure = "the provider refused the request"

// landInlined sends a capture with the inputs and extra of every model call
// that arrived before it finished moved inside its envelope, and lands it. It returns how
// many calls it rewrote.
func landInlined(t *testing.T, zone *storage.Zone, kase string) int {
	t.Helper()
	receiver := &langsmith.Receiver{Zone: zone, Listen: "127.0.0.1:0", Now: time.Now}
	if err := receiver.Start(); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	defer receiver.Stop()
	dir := filepath.Join(corpus, kase)
	requests, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no corpus for %s: %v", kase, err)
	}
	inlined := 0
	for _, r := range requests {
		raw, err := os.ReadFile(filepath.Join(dir, r.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(dir, r.Name(), "body.bin"))
		if err != nil {
			t.Fatal(err)
		}
		body, contentType, n := inlineUnfinished(t, body, meta.Headers["Content-Type"])
		inlined += n
		req, _ := http.NewRequest(http.MethodPost, "http://"+receiver.Addr()+"/runs/multipart", bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", r.Name(), err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status %d", r.Name(), resp.StatusCode)
		}
	}
	collector := &langsmith.Collector{Zone: zone, Now: time.Now}
	if _, err := collector.Collect(); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return inlined
}

// inlineUnfinished rewrites one multipart request: the inputs and extra parts
// of a model run with no end time go inside its envelope.
func inlineUnfinished(t *testing.T, body []byte, contentType string) ([]byte, string, int) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	type part struct {
		name string
		data []byte
	}
	var parts []part
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, part{p.FormName(), data})
	}
	byName := map[string][]byte{}
	for _, p := range parts {
		byName[p.name] = p.data
	}
	moved := map[string]bool{}
	inlined := 0
	for i, p := range parts {
		if strings.Count(p.name, ".") != 1 {
			continue
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(p.data, &envelope) != nil {
			continue
		}
		var kind, end string
		_ = json.Unmarshal(envelope["run_type"], &kind)
		_ = json.Unmarshal(envelope["end_time"], &end)
		if (kind != "llm" && kind != "chat_model") || end != "" {
			continue
		}
		for _, field := range []string{"inputs", "extra"} {
			if data, ok := byName[p.name+"."+field]; ok {
				envelope[field] = data
				moved[p.name+"."+field] = true
			}
		}
		// It failed as well, so its record carries text of its own, the
		// failure, which the whole document shows beside the request.
		envelope["error"] = json.RawMessage(`"` + callFailure + `"`)
		parts[i].data, _ = json.Marshal(envelope)
		inlined++
	}
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	for _, p := range parts {
		if moved[p.name] {
			continue
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, p.name))
		header.Set("Content-Type", "application/json")
		w, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(p.data)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), writer.FormDataContentType(), inlined
}

// TestARunsOwnRecordIsCountedWhenWithheld lands a prompt run under a
// decorated function with no model call in the trace, as a function that
// builds its own messages is traced. The function's record and the prompt's
// are named for the system message they hold, and no step stands on either:
// the fold draws nothing for a run's own record. The record endpoint
// withholds them all the same, so the document counts them, where a zero
// would say the filter found nothing.
func TestARunsOwnRecordIsCountedWhenWithheld(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "31313131-3131-7131-8131-313131313130"
	const prompt = "31313131-3131-7131-8131-313131313131"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-prompt","ls_method":"traceable"}}`
	root := "20260920T100000000000Z" + trace
	messages := `[{"role":"system","content":"You are a careful assistant."},{"role":"user","content":"what is the status"}]`
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `",` +
		`"dotted_order":"` + root + `","run_type":"chain","name":"build_prompt",` +
		`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:10Z",` + owner + `,` +
		`"inputs":{"question":"what is the status"},"outputs":{"output":{"messages":` + messages + `}}},` +
		`{"id":"` + prompt + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + root + `.20260920T100001000000Z` + prompt + `","run_type":"prompt","name":"ChatPromptTemplate",` +
		`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:01Z",` + owner + `,` +
		`"inputs":{"question":"what is the status"},` +
		`"outputs":{"output":{"messages":` + messages + `}}}]}`)
	session := collect().Sessions[0]
	if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The named records, by position, read from the landed files.
	named := map[[2]uint64]bool{}
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		rd, err := sessiondata.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		for row := uint64(1); ; row++ {
			rec, rerr := rd.Next()
			if rerr != nil {
				break
			}
			if slices.Contains(rec.Flags, sessiondata.FlagSystemPrompt) {
				named[[2]uint64{lf.Seq, row}] = true
			}
		}
		f.Close()
	}
	if len(named) != 2 {
		t.Fatalf("%d records are named system_prompt, want the function's and the prompt's", len(named))
	}
	c, err := view_.New(zone, nil).Load(session)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.BuildWithheld([]string{sessiondata.FlagSystemPrompt})
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Summary.Withheld[sessiondata.FlagSystemPrompt]; got != len(named) {
		t.Fatalf("withheld %v, want %d under system_prompt", doc.Summary.Withheld, len(named))
	}
	var walk func([]sessionview.Node)
	walk = func(nodes []sessionview.Node) {
		for i := range nodes {
			if ref := nodes[i].Ref; ref != nil && named[[2]uint64{ref.Seq, ref.Row}] {
				t.Fatalf("step %s stands on a named record, so this test no longer checks a record with no step", nodes[i].ID)
			}
			walk(nodes[i].Children)
		}
	}
	walk(doc.Talks)
	walk(doc.Loose)
	h := view_.New(zone, nil).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		return r
	}
	for pos := range named {
		address := fmt.Sprintf("/api/c/%s/record/%d/%d", session, pos[0], pos[1])
		if r := get(address); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "careful assistant") {
			t.Fatalf("the whole record %v does not show the system message: %d %.300s", pos, r.Code, r.Body.String())
		}
		var rec sessiondata.Record
		r := get(address + "?hide=system_prompt")
		if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &rec) != nil || len(rec.Parts) == 0 {
			t.Fatalf("the withheld record %v: %d %.300s", pos, r.Code, r.Body.String())
		}
		for _, p := range rec.Parts {
			if len(p.Data) != 0 || p.Text != "" || p.State != "omitted" {
				t.Fatalf("the withheld record %v kept content: %+v", pos, p)
			}
		}
	}
}
