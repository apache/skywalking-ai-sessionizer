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

package langsmith

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// collectorOver is a collector on a root, with the clock fixed so a test that
// compares landed names compares the same names every run.
func collectorOver(zone *storage.Zone) *Collector {
	at := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	return &Collector{Zone: zone, Now: func() time.Time { at = at.Add(time.Second); return at }}
}

// TestAGzipBodyIsBoundedByWhatItBecomes.
//
// The size limit is applied to what arrives, which for a compressed body is
// not the size of the body. A few hundred kilobytes of zeros expands past
// every bound this receiver has, and read it all into memory before anything
// looked at how big it was.
func TestAGzipBodyIsBoundedByWhatItBecomes(t *testing.T) {
	_, _, base := started(t, "")

	var packed bytes.Buffer
	gz := gzip.NewWriter(&packed)
	// Well past MaxRequest once decoded, and small enough to send.
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for written := 0; written <= MaxRequest; written += len(chunk) {
		if _, err := gz.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d compressed bytes become more than the %d this receiver accepts",
		packed.Len(), MaxRequest)

	req, err := http.NewRequest(http.MethodPost, base+"/runs/multipart", bytes.NewReader(packed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// TestASingleRunLandsAndAPatchEndsIt.
//
// POST /runs and PATCH /runs/{id} carry one run rather than a batch. Both
// were accepted with a 202 and then dropped by the collector, so a client
// using the documented endpoints was told its data was kept and it was not.
//
// The update is the harder half. The client sends update_run(id, outputs=...)
// as the id and the outputs alone, with the trace, the project, the kind of
// run and the dotted order all null. Read on its own that is a run belonging
// to no conversation, of no kind: it landed under a name saying its identity
// was not supplied, was treated as framework bookkeeping, and its output was
// dropped, while the call it was ending stayed unfinished for ever.
func TestASingleRunLandsAndAPatchEndsIt(t *testing.T) {
	_, zone, base := started(t, "")
	const id = "11111111-1111-7111-8111-111111111111"

	// The run starts. No end time: this is work in progress.
	post := `{"id":"` + id + `","trace_id":"` + id + `",` +
		`"dotted_order":"20260920T100000000000Z` + id + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:00Z",` +
		`"session_name":"asz","extra":{"metadata":{"thread_id":"t-solo"}}}`
	resp := send(t, base, "/runs", "application/json", []byte(post), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("post status %d", resp.StatusCode)
	}
	collector := collectorOver(zone)
	landed, err := collector.Collect()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if landed.Records == 0 {
		t.Fatal("a single run was accepted and then landed nothing")
	}
	if landed.Unassigned != 0 {
		t.Errorf("%d unassigned, want 0: the run supplied a thread id", landed.Unassigned)
	}
	if len(landed.Sessions) != 1 || !strings.Contains(landed.Sessions[0], "t-solo") {
		t.Fatalf("landed in %v, want one session named for the supplied thread", landed.Sessions)
	}
	session := landed.Sessions[0]

	// And it ends, the way the client's own update_run sends it.
	patch := `{"id":"` + id + `","outputs":{"generations":[[{"message":{"kwargs":` +
		`{"content":"the answer","usage_metadata":{"input_tokens":11,"output_tokens":3}}}}]]},` +
		`"end_time":"2026-09-20T10:00:05Z","trace_id":null,"run_type":null,` +
		`"session_name":null,"dotted_order":null}`
	req, err := http.NewRequest(http.MethodPatch, base+"/runs/"+id, strings.NewReader(patch))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("patch status %d", resp.StatusCode)
	}
	landed, err = collector.Collect()
	if err != nil {
		t.Fatalf("collect the update: %v", err)
	}
	if landed.Unassigned != 0 {
		t.Errorf("the update landed unassigned: it names the run its start already placed")
	}
	if len(landed.Sessions) != 1 || landed.Sessions[0] != session {
		t.Fatalf("the update landed in %v, want the same session as its start, %s",
			landed.Sessions, session)
	}

	// The answer it carried has to be in the conversation, on a finished
	// model call. Landing it as framework data lost both.
	found := false
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for {
			record, err := reader.Next()
			if err != nil {
				break
			}
			for _, part := range record.Parts {
				if part.Text != "the answer" {
					continue
				}
				found = true
				if record.Call != id {
					t.Errorf("the answer landed on %q, not on the call it ended", record.Call)
				}
				if record.Usage == nil || record.Usage.Output != 3 {
					t.Errorf("the usage the update reported did not land: %+v", record.Usage)
				}
			}
		}
		f.Close()
	}
	if !found {
		t.Error("the update's answer is nowhere in the conversation")
	}
}

// TestABodyWithNoRunInItIsRefused: answering 202 to a body nothing can be
// read out of tells the client it was kept when it was not.
func TestABodyWithNoRunInItIsRefused(t *testing.T) {
	_, _, base := started(t, "")
	for _, body := range []string{`"just a string"`, `42`, `{"no":"id"}`} {
		resp := send(t, base, "/runs", "application/json", []byte(body), "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want %d", body, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

// TestOneUnreadableRequestDoesNotStopTheRest.
//
// A body that cannot be read will not read better next time. Leaving it at
// the head of the inbox stopped every request behind it from ever being
// collected, so one bad body took the whole root quiet and nothing said why.
func TestOneUnreadableRequestDoesNotStopTheRest(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	inbox := NewInbox(zone)
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	// A body already in the inbox that nothing can be read out of. The
	// receiver refuses such a body now, so this stands for one written by an
	// older build, or one a filesystem damaged after it was accepted.
	if _, err := inbox.Put("application/json", http.MethodPost, "/runs/batch",
		[]byte(`{"post":[{"id":`), at); err != nil {
		t.Fatal(err)
	}
	good := `{"post":[{"id":"22222222-2222-7222-8222-222222222222",` +
		`"trace_id":"22222222-2222-7222-8222-222222222222",` +
		`"dotted_order":"20260920T100001000000Z22222222-2222-7222-8222-222222222222",` +
		`"run_type":"chain","name":"after","start_time":"2026-09-20T10:00:01Z",` +
		`"end_time":"2026-09-20T10:00:02Z","session_name":"asz",` +
		`"extra":{"metadata":{"thread_id":"t-after"}},` +
		`"inputs":{"messages":[{"role":"user","content":"still here"}]}}]}`
	if _, err := inbox.Put("application/json", http.MethodPost, "/runs/batch",
		[]byte(good), at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if landed.Unreadable != 1 {
		t.Errorf("%d unreadable, want 1", landed.Unreadable)
	}
	if landed.Requests != 1 || landed.Records == 0 {
		t.Fatalf("the request behind the bad one landed %d records from %d requests",
			landed.Records, landed.Requests)
	}

	// The bad request is kept, with the reason beside it, so someone can see
	// what arrived rather than guess.
	aside, err := os.ReadDir(filepath.Join(inbox.Dir(), UnreadableDir))
	if err != nil {
		t.Fatal(err)
	}
	var reasons int
	for _, f := range aside {
		if strings.HasSuffix(f.Name(), ".reason") {
			reasons++
		}
	}
	if reasons != 1 {
		t.Errorf("%d reasons beside %d moved requests", reasons, len(aside)-reasons)
	}

	// And the inbox is empty, so the next pass does not meet it again.
	rest, err := inbox.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Errorf("%d requests still waiting", len(rest))
	}
}

// TestIdentityIsFoundWhenTheClientKeepsExtraInline.
//
// The client splits extra into a part of its own only when it is large enough
// to be worth one. A small run keeps it in the envelope, and reading that as
// though it were already the metadata found no supplied key, so every such
// conversation landed as one whose identity was not supplied and merged with
// nothing.
func TestIdentityIsFoundWhenTheClientKeepsExtraInline(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	inbox := NewInbox(zone)
	body := `{"post":[{"id":"33333333-3333-7333-8333-333333333333",` +
		`"trace_id":"33333333-3333-7333-8333-333333333333",` +
		`"dotted_order":"20260920T100000000000Z33333333-3333-7333-8333-333333333333",` +
		`"run_type":"chain","name":"inline","start_time":"2026-09-20T10:00:00Z",` +
		`"end_time":"2026-09-20T10:00:01Z","session_name":"proj",` +
		`"extra":{"metadata":{"thread_id":"t-inline"},"runtime":{"sdk":"langsmith-py"}},` +
		`"inputs":{"messages":[{"role":"user","content":"hello"}]}}]}`
	if _, err := inbox.Put("application/json", http.MethodPost, "/runs/batch",
		[]byte(body), time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Unassigned != 0 {
		t.Fatalf("%d unassigned: the thread id was in the envelope's extra", landed.Unassigned)
	}
	want := StorageID(Owner{Values: []string{"proj", "t-inline"}})
	if len(landed.Sessions) != 1 || landed.Sessions[0] != want {
		t.Fatalf("landed in %v, want %s", landed.Sessions, want)
	}
}

// TestToolArgumentsKeepTheirSourceBytes.
//
// A landed part that carries the source's own JSON has to carry the source's
// own bytes. Reading the arguments into a map and encoding them again turned
// every number into a float64, so an argument no float64 can hold came back
// changed and a reader replaying the call would send something that was never
// sent.
func TestToolArgumentsKeepTheirSourceBytes(t *testing.T) {
	const big = "12345678901234567890"
	outputs := `{"generations":[[{"message":{"kwargs":{"content":"",` +
		`"tool_calls":[{"id":"call_1","name":"lookup","args":{"n":` + big + `,"q":0.1}}]}}}]]}`
	op := Operation{Op: "post", RunID: "r1",
		Envelope: json.RawMessage(`{"id":"r1","trace_id":"t1","run_type":"llm",` +
			`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:01Z"}`),
		Fields: map[string]json.RawMessage{"outputs": json.RawMessage(outputs)}}
	records, err := Convert(op, false, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range records {
		for _, part := range r.Parts {
			if part.Kind != "call" {
				continue
			}
			found = true
			if !bytes.Contains(part.Data, []byte(big)) {
				t.Errorf("arguments landed as %s, want the source's own %s", part.Data, big)
			}
		}
	}
	if !found {
		t.Fatal("no call part was produced")
	}
}

// TestAFailureIsNotJoinedToTheWrongCall.
//
// A tool that raised names no call, so the call is looked for among the ones
// the model asked for and nothing has answered. Three ways that goes wrong,
// and all three are refusals rather than guesses:
//
//   - a conversation that asks for the same tool twice has several, and
//     taking the only unanswered one joined the failure to the NEXT call
//     rather than the one that failed;
//   - the order cannot come from the dotted order, which is the shape of the
//     tree: each branch carries its own start, so a model call that ran late
//     inside a branch that started early sorts before a tool that ran first;
//   - a call made inside a nested agent is not the caller's to answer.
func TestAFailureIsNotJoinedToTheWrongCall(t *testing.T) {
	const trace = "t1"
	// Every id below is a real one, because the ancestry in a dotted order
	// is what says which agent a run belongs to.
	const rootID = "aaaaaaaa-0000-7000-8000-00000000root"
	const earlyID = "aaaaaaaa-0000-7000-8000-000000000001"
	const lateID = "aaaaaaaa-0000-7000-8000-000000000002"
	const failedID = "aaaaaaaa-0000-7000-8000-000000000003"
	const branchID = "aaaaaaaa-0000-7000-8000-000000000004"
	const nestedID = "aaaaaaaa-0000-7000-8000-000000000005"
	const agentToolID = "aaaaaaaa-0000-7000-8000-00000000000a"
	root := "20260920T100000000000Z" + rootID

	model := func(id, dotted, call, start, end string) Operation {
		return Operation{Op: "post", RunID: id, Envelope: json.RawMessage(
			`{"id":"` + id + `","trace_id":"` + trace + `","run_type":"llm",` +
				`"dotted_order":"` + dotted + `",` +
				`"start_time":"` + start + `","end_time":"` + end + `"}`),
			Fields: map[string]json.RawMessage{"outputs": json.RawMessage(
				`{"generations":[[{"message":{"kwargs":{"tool_calls":[{"id":"` + call +
					`","name":"lookup","args":{}}]}}}]]}`)}}
	}
	failed := func(id, dotted, start, end string) Operation {
		return Operation{Op: "patch", RunID: id, Envelope: json.RawMessage(
			`{"id":"` + id + `","trace_id":"` + trace + `","run_type":"tool","name":"lookup",` +
				`"dotted_order":"` + dotted + `",` +
				`"start_time":"` + start + `","end_time":"` + end + `"}`),
			Fields: map[string]json.RawMessage{
				"outputs": json.RawMessage(`{}`),
				"error":   json.RawMessage(`"it raised"`)}}
	}

	t.Run("the call made before it, not the one after", func(t *testing.T) {
		hints := Resolve([]Operation{
			model(earlyID, root+".20260920T100001000000Z"+earlyID, "call_early",
				"2026-09-20T10:00:01Z", "2026-09-20T10:00:02Z"),
			model(lateID, root+".20260920T100008000000Z"+lateID, "call_late",
				"2026-09-20T10:00:08Z", "2026-09-20T10:00:09Z"),
			failed(failedID, root+".20260920T100005000000Z"+failedID,
				"2026-09-20T10:00:05Z", "2026-09-20T10:00:06Z"),
		})
		if got := hints.ToolCall[failedID]; got != "call_early" {
			t.Errorf("joined to %q, want call_early", got)
		}
	})

	t.Run("a late call in a branch that started early", func(t *testing.T) {
		// The branch starts at 10:00:01, so every dotted order under it
		// sorts before the tool's. The model call inside it runs at
		// 10:00:09, after the tool already failed at 10:00:06.
		branch := root + ".20260920T100001000000Z" + branchID
		hints := Resolve([]Operation{
			model(lateID, branch+".20260920T100009000000Z"+lateID, "call_late",
				"2026-09-20T10:00:09Z", "2026-09-20T10:00:10Z"),
			failed(failedID, root+".20260920T100005000000Z"+failedID,
				"2026-09-20T10:00:05Z", "2026-09-20T10:00:06Z"),
		})
		if got := hints.ToolCall[failedID]; got != "" {
			t.Errorf("joined to %q, which the model asked for after the tool had run", got)
		}
	})

	t.Run("a call made inside a nested agent", func(t *testing.T) {
		// The model call runs inside a tool, which makes it a different
		// agent's. It is not the caller's to answer, however well the name
		// and the times line up.
		inside := root + ".20260920T100001000000Z" + agentToolID
		hints := Resolve([]Operation{
			{Op: "post", RunID: agentToolID, Envelope: json.RawMessage(
				`{"id":"` + agentToolID + `","trace_id":"` + trace + `","run_type":"tool",` +
					`"name":"delegate","dotted_order":"` + inside + `",` +
					`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:04Z"}`),
				Fields: map[string]json.RawMessage{}},
			model(nestedID, inside+".20260920T100002000000Z"+nestedID, "call_nested",
				"2026-09-20T10:00:02Z", "2026-09-20T10:00:03Z"),
			failed(failedID, root+".20260920T100005000000Z"+failedID,
				"2026-09-20T10:00:05Z", "2026-09-20T10:00:06Z"),
		})
		if got := hints.ToolCall[failedID]; got != "" {
			t.Errorf("joined to %q, which a nested agent asked for", got)
		}
	})
}

// TestAStreamDiscoveredAfterItsCallLandedIsStillJoined.
//
// A tool's own records can land before anything is known to have run inside
// it: the tool is posted and collected, and the nested agent's runs arrive in
// a later request. Only the records of the request being converted are
// marked, so the tool said nothing about the stream that turned out to start
// there. The child stream was real, nothing named the call that started it,
// and assembly reported it as an orphan for ever.
func TestAStreamDiscoveredAfterItsCallLandedIsStillJoined(t *testing.T) {
	_, zone, base := started(t, "")
	const trace = "66666666-6666-7666-8666-666666666666"
	const toolID = "66666666-6666-7666-8666-666666666661"
	const nested = "66666666-6666-7666-8666-666666666662"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-late"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100001000000Z" + toolID

	// First request: the trace's root, and a tool that ran and finished.
	// Nothing has run inside the tool yet.
	first := `{"post":[{"id":"` + trace + `","trace_id":"` + trace + `",` +
		`"dotted_order":"` + root + `","run_type":"chain","name":"agent",` +
		`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:10Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"delegate this"}]}},` +
		`{"id":"` + toolID + `","trace_id":"` + trace + `",` +
		`"parent_run_id":"` + trace + `","dotted_order":"` + toolDotted + `",` +
		`"run_type":"tool","name":"delegate","start_time":"2026-09-20T10:00:01Z",` +
		`"end_time":"2026-09-20T10:00:09Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"call_delegate_1","content":"done"}}}]}`
	resp := send(t, base, "/runs/batch", "application/json", []byte(first), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first status %d", resp.StatusCode)
	}
	collector := collectorOver(zone)
	if _, err := collector.Collect(); err != nil {
		t.Fatal(err)
	}

	// Second request: a model call that ran inside it.
	second := `{"post":[{"id":"` + nested + `","trace_id":"` + trace + `",` +
		`"parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100002000000Z` + nested + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:02Z",` +
		`"end_time":"2026-09-20T10:00:08Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"nested answer"}}}]]}}]}`
	resp = send(t, base, "/runs/batch", "application/json", []byte(second), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second status %d", resp.StatusCode)
	}
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	session := landed.Sessions[0]

	// The nested call is in a stream of its own, and something in the
	// session names the call that started it.
	child := StreamName(toolID, "delegate")
	streams, joined := map[string]bool{}, false
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		streams[reader.Header().Stream] = true
		for {
			record, err := reader.Next()
			if err != nil {
				break
			}
			if record.Child == child && record.Tool == "call_delegate_1" {
				joined = true
			}
		}
		f.Close()
	}
	if !streams[child] {
		t.Errorf("the nested call is not in its own stream; streams are %v", keysOf(streams))
	}
	if !joined {
		t.Error("nothing names the call that started the child stream, so it is an orphan")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTwoArrivalsCannotShareARecordID.
//
// A record id is the receipt for one arrival. It is built from the bytes the
// arrival came with, and writing each field's name straight before its value
// runs the two together: the fields {"a":1,"b":2} and {"a1b":2} both write
// a1b2. Two arrivals then share an id, and the index keeps the first entry
// for an id, so the second is read as a repeat and dropped.
func TestTwoArrivalsCannotShareARecordID(t *testing.T) {
	envelope := json.RawMessage(`{"id":"r","trace_id":"t","run_type":"tool"}`)
	one := Operation{Op: "patch", RunID: "r", Envelope: envelope,
		Fields: map[string]json.RawMessage{
			"a": json.RawMessage(`1`), "b": json.RawMessage(`2`)}}
	two := Operation{Op: "patch", RunID: "r", Envelope: envelope,
		Fields: map[string]json.RawMessage{"a1b": json.RawMessage(`2`)}}
	if RecordID(one) == RecordID(two) {
		t.Fatalf("two different arrivals share the id %s", RecordID(one))
	}
}

// TestAStartAndAnUpdateInOneBatchAreBothThemselves.
//
// A batch can carry a run's start and an update to it together. Keeping one
// envelope per run kept only the last of them, and both arrivals were then
// read as the update - so the start's kind, trace and project were lost from
// the very record that had carried them.
func TestAStartAndAnUpdateInOneBatchAreBothThemselves(t *testing.T) {
	_, zone, base := started(t, "")
	const id = "77777777-7777-7777-8777-777777777777"
	body := `{"post":[{"id":"` + id + `","trace_id":"` + id + `",` +
		`"dotted_order":"20260920T100000000000Z` + id + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:00Z",` +
		`"session_name":"asz","extra":{"metadata":{"thread_id":"t-both"}}}],` +
		`"patch":[{"id":"` + id + `","end_time":"2026-09-20T10:00:04Z",` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"done"}}}]]}}]}`
	resp := send(t, base, "/runs/batch", "application/json", []byte(body), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Unassigned != 0 {
		t.Errorf("%d unassigned: the start in the same batch names the conversation", landed.Unassigned)
	}
	if len(landed.Sessions) != 1 || !strings.Contains(landed.Sessions[0], "t-both") {
		t.Fatalf("landed in %v, want the session the start named", landed.Sessions)
	}

	// Both arrivals are there, and the one that ended it carries the answer
	// on the call it ended.
	var ids int
	var answered bool
	files, err := storage.LandedFiles(zone, landed.Sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for {
			record, err := reader.Next()
			if err != nil {
				break
			}
			if record.Call == id {
				ids++
			}
			for _, part := range record.Parts {
				if part.Text == "done" && record.Call == id {
					answered = true
				}
			}
		}
		f.Close()
	}
	if ids < 2 {
		t.Errorf("%d records for the run, want both its arrivals", ids)
	}
	if !answered {
		t.Error("the answer did not land on the call it ended")
	}
}

// TestRunsWithNoTraceAreNotOneConversation.
//
// A client can create a run with neither a thread key nor a trace. Naming
// every such run after the empty string put all of them in one conversation,
// and their opening messages all shared one record id, so the index read the
// later ones as repeats and dropped them. Unrelated work then read as one
// person's session with most of its questions missing.
func TestRunsWithNoTraceAreNotOneConversation(t *testing.T) {
	_, zone, base := started(t, "")
	ask := func(id, question string) {
		t.Helper()
		body := `{"post":[{"id":"` + id + `","run_type":"chain","name":"ask",` +
			`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:01Z",` +
			`"inputs":{"messages":[{"role":"user","content":"` + question + `"}]}}]}`
		resp := send(t, base, "/runs/batch", "application/json", []byte(body), "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	ask("88888888-8888-7888-8888-888888888881", "first question")
	ask("88888888-8888-7888-8888-888888888882", "second question")

	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Unassigned != 2 {
		t.Errorf("%d unassigned, want 2: neither run supplied a conversation", landed.Unassigned)
	}
	if len(landed.Sessions) != 2 {
		t.Fatalf("two unrelated runs landed in %v, want a session each", landed.Sessions)
	}
	// And each keeps its own question.
	for _, session := range landed.Sessions {
		if !mentions(t, zone, session, "question") {
			t.Errorf("%s carries no question", session)
		}
	}
}

// TestAModelCallInAnEarlierRequestStillCounts.
//
// Whether a trace has a model call in it is a fact about the trace, not
// about one request. A batch carrying a root's completion and a failed tool
// looks like a trace with no model in it, and a tool that names no call then
// had one made up for it - a second call for one real execution, unanswered,
// beside the real one.
func TestAModelCallInAnEarlierRequestStillCounts(t *testing.T) {
	_, zone, base := started(t, "")
	const trace = "99999999-9999-7999-8999-999999999990"
	const llm = "99999999-9999-7999-8999-999999999991"
	const tool = "99999999-9999-7999-8999-999999999992"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-late-model","ls_method":"traceable"}}`
	root := "20260920T100000000000Z" + trace

	// The trace starts, and a model call runs in it.
	first := `{"post":[{"id":"` + trace + `","trace_id":"` + trace + `",` +
		`"dotted_order":"` + root + `","run_type":"chain","name":"wrapped",` +
		`"start_time":"2026-09-20T10:00:00Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"go"}]}},` +
		`{"id":"` + llm + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + root + `.20260920T100001000000Z` + llm + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:01Z",` +
		`"end_time":"2026-09-20T10:00:02Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"tool_calls":` +
		`[{"id":"call_look_1","name":"look","args":{}}]}}}]]}}]}`
	resp := send(t, base, "/runs/batch", "application/json", []byte(first), "")
	resp.Body.Close()
	collector := collectorOver(zone)
	if _, err := collector.Collect(); err != nil {
		t.Fatal(err)
	}

	// Then a batch with the root's completion and a tool that raised. On its
	// own this looks like a trace with no model call in it.
	second := `{"patch":[{"id":"` + trace + `","end_time":"2026-09-20T10:00:09Z"}],` +
		`"post":[{"id":"` + tool + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + root + `.20260920T100003000000Z` + tool + `",` +
		`"run_type":"tool","name":"look","start_time":"2026-09-20T10:00:03Z",` +
		`"end_time":"2026-09-20T10:00:04Z",` + owner + `,` +
		`"outputs":{},"error":"it raised"}]}`
	resp = send(t, base, "/runs/batch", "application/json", []byte(second), "")
	resp.Body.Close()
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}

	// No call may be made up. A made-up one is named after the tool run
	// itself, so it is told apart from the real one the model asked for.
	for _, session := range landed.Sessions {
		if callWithID(t, zone, session, tool) {
			t.Error("a call was made up for a tool a model had already asked for")
		}
		if !callWithID(t, zone, session, "call_look_1") {
			t.Error("the call the model really made is not there")
		}
	}
}

// callWithID reports whether the session holds a call part with this id.
func callWithID(t *testing.T, zone *storage.Zone, session, id string) bool {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for {
			record, err := reader.Next()
			if err != nil {
				break
			}
			for _, part := range record.Parts {
				if part.Kind == sessiondata.PartCall && part.ID == id {
					f.Close()
					return true
				}
			}
		}
		f.Close()
	}
	return false
}

// mentions reports whether any landed file of the session holds the text.
func mentions(t *testing.T, zone *storage.Zone, session, text string) bool {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), text) {
			return true
		}
	}
	return false
}

// TestAChildThatOvertookItsParentIsNotGivenToIt.
//
// The client drains its queue with several threads at once, so a batch can
// reach the receiver before the one carrying the runs it ran inside. Placing
// it then is a guess, and the guess was always the parent lineage - which
// gives an independent agent's model context and its answers to its caller.
// That is not a thin conversation, it is a wrong one.
//
// So a request whose ancestry has not arrived waits. It is landed anyway
// once it has waited as long as it may, because a trace that never completes
// must not hold its evidence out of the conversation for ever.
func TestAChildThatOvertookItsParentIsNotGivenToIt(t *testing.T) {
	_, zone, base := started(t, "")
	const trace = "12121212-1212-7212-8212-121212121210"
	const toolID = "12121212-1212-7212-8212-121212121211"
	const nested = "12121212-1212-7212-8212-121212121212"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-overtake"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100001000000Z" + toolID

	// The nested agent's model call arrives FIRST, before anything that says
	// what it ran inside.
	child := `{"post":[{"id":"` + nested + `","trace_id":"` + trace + `",` +
		`"parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100002000000Z` + nested + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:02Z",` +
		`"end_time":"2026-09-20T10:00:08Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"the analyst answer"}}}]]}}]}`
	resp := send(t, base, "/runs/batch", "application/json", []byte(child), "")
	resp.Body.Close()
	collector := collectorOver(zone)
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Waiting != 1 {
		t.Fatalf("%d requests waiting, want 1: its ancestry has not arrived", landed.Waiting)
	}
	if landed.Records != 0 {
		t.Fatalf("%d records landed before it was known where they belong", landed.Records)
	}

	// Then the batch that overtook it.
	parent := `{"post":[{"id":"` + trace + `","trace_id":"` + trace + `",` +
		`"dotted_order":"` + root + `","run_type":"chain","name":"agent",` +
		`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:10Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"delegate this"}]}},` +
		`{"id":"` + toolID + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + toolDotted + `","run_type":"tool","name":"delegate",` +
		`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:09Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"call_delegate_1","content":"done"}}}]}`
	resp = send(t, base, "/runs/batch", "application/json", []byte(parent), "")
	resp.Body.Close()
	landed, err = collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Waiting != 0 {
		t.Errorf("%d still waiting once its ancestry arrived", landed.Waiting)
	}
	session := landed.Sessions[0]

	// The nested answer belongs to the agent that produced it.
	want := StreamName(toolID, "delegate")
	where := streamHolding(t, zone, session, "the analyst answer")
	if where != want {
		t.Errorf("the nested agent's answer landed in %q, want its own stream %q", where, want)
	}
}

// streamHolding says which stream's landed file carries the text.
func streamHolding(t *testing.T, zone *storage.Zone, session, text string) string {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), text) {
			continue
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(f)
		if err == nil {
			stream := reader.Header().Stream
			f.Close()
			return stream
		}
		f.Close()
	}
	return ""
}

// TestATracedToolsCallIsNotInvented.
//
// A decorated function calls its tools itself, so no model asks for them and
// nothing in the trace carries a call. Making one up from the tool run gave
// those traces a tool step, and it could not be made safely: ls_method is
// inherited, so a decorated function wrapping a graph marks that graph's own
// tools as traced too, and no test on one request tells a trace with no
// model call from one whose model call has not arrived yet.
//
// So it is not made. What the tool was given and what it returned are landed
// either way; what is missing is the step joining them.
func TestATracedToolsCallIsNotInvented(t *testing.T) {
	op := Operation{Op: "post", RunID: "tool-1", Envelope: json.RawMessage(
		`{"id":"tool-1","trace_id":"tr","run_type":"tool","name":"lookup",` +
			`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:01Z"}`),
		Fields: map[string]json.RawMessage{
			"inputs":  json.RawMessage(`{"cluster":"prod-1"}`),
			"outputs": json.RawMessage(`{"output":"prod-1: 3/3 ready"}`),
			"extra":   json.RawMessage(`{"metadata":{"ls_method":"traceable"}}`)}}
	records, err := Convert(op, false, Hints{SelfContained: map[string]bool{"tr": true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		for _, part := range record.Parts {
			if part.Kind == sessiondata.PartCall {
				t.Errorf("a call was invented: %s %q", part.ID, part.Name)
			}
		}
	}
	// The result is still landed, with what the tool returned in it.
	var landed bool
	for _, record := range records {
		for _, part := range record.Parts {
			if part.Kind == sessiondata.PartResult && strings.Contains(part.Text, "3/3 ready") {
				landed = true
			}
		}
	}
	if !landed {
		t.Error("the tool's result did not land, so nothing of it is in the conversation")
	}
}

// TestAncestryThatArrivesOutOfOrderInOnePassStillPlaces.
//
// Three requests, in the order a concurrent sender can produce them: the
// nested agent's model call, then the tool it ran inside, then the trace's
// root. Everything arrives within one pass.
//
// Deciding a run may be placed and then placing it used two different
// memories: the first knew the tool had been seen, the second knew only what
// had landed. The child passed the first and failed the second, and went to
// the parent lineage although all of its ancestry was there.
func TestAncestryThatArrivesOutOfOrderInOnePassStillPlaces(t *testing.T) {
	_, zone, base := started(t, "")
	const trace = "13131313-1313-7313-8313-131313131310"
	const toolID = "13131313-1313-7313-8313-131313131311"
	const nested = "13131313-1313-7313-8313-131313131312"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-order"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100001000000Z" + toolID

	post := func(body string) {
		t.Helper()
		resp := send(t, base, "/runs/batch", "application/json", []byte(body), "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	// The child first.
	post(`{"post":[{"id":"` + nested + `","trace_id":"` + trace + `",` +
		`"parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100002000000Z` + nested + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:02Z",` +
		`"end_time":"2026-09-20T10:00:08Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"nested answer"}}}]]}}]}`)
	// Then the tool it ran inside.
	post(`{"post":[{"id":"` + toolID + `","trace_id":"` + trace + `",` +
		`"parent_run_id":"` + trace + `","dotted_order":"` + toolDotted + `",` +
		`"run_type":"tool","name":"delegate","start_time":"2026-09-20T10:00:01Z",` +
		`"end_time":"2026-09-20T10:00:09Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"call_delegate_1","content":"done"}}}]}`)
	// Then the root.
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `",` +
		`"dotted_order":"` + root + `","run_type":"chain","name":"agent",` +
		`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:10Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"delegate this"}]}}]}`)

	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Waiting != 0 {
		t.Errorf("%d waiting although every run arrived in this pass", landed.Waiting)
	}
	want := StreamName(toolID, "delegate")
	if got := streamHolding(t, zone, landed.Sessions[0], "nested answer"); got != want {
		t.Errorf("the nested answer landed in %q, want its own stream %q", got, want)
	}
}

// TestANameFromARunGivesWayToTheQuestion.
//
// A root's completion can arrive before its start, and then nothing has
// been asked yet: the only name to hand is the run's, "LangGraph", which is
// the program and not the conversation. That name was landed and locked, so
// the question, arriving next, could never replace it.
//
// A name taken from a run gives way to the question. A question gives way
// to nothing.
func TestANameFromARunGivesWayToTheQuestion(t *testing.T) {
	_, zone, base := started(t, "")
	const trace = "31313131-3131-7131-8131-313131313130"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-named"}}`
	root := "20260920T100000000000Z" + trace

	// The completion first: identity, a name, no inputs.
	resp := send(t, base, "/runs/batch", "application/json", []byte(
		`{"patch":[{"id":"`+trace+`","trace_id":"`+trace+`","dotted_order":"`+root+`",`+
			`"run_type":"chain","name":"LangGraph","end_time":"2026-09-20T10:00:09Z",`+owner+`}]}`), "")
	resp.Body.Close()
	collector := collectorOver(zone)
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	session := landed.Sessions[0]
	if got := titlesOf(t, zone, session); len(got) != 1 || got[0] != "LangGraph" {
		t.Fatalf("named %v after the completion, want only the run's name", got)
	}

	// Then the start, with the question.
	resp = send(t, base, "/runs/batch", "application/json", []byte(
		`{"post":[{"id":"`+trace+`","trace_id":"`+trace+`","dotted_order":"`+root+`",`+
			`"run_type":"chain","name":"LangGraph","start_time":"2026-09-20T10:00:00Z",`+owner+`,`+
			`"inputs":{"messages":[{"role":"user","content":"Is prod-1 healthy?"}]}}]}`), "")
	resp.Body.Close()
	if _, err := collector.Collect(); err != nil {
		t.Fatal(err)
	}
	got := titlesOf(t, zone, session)
	if len(got) == 0 || got[len(got)-1] != "Is prod-1 healthy?" {
		t.Fatalf("named %v, want the question last", got)
	}
}

// titlesOf lists the names landed for a session, in landed order. A name
// is a record carrying a label and no identity of its own.
func titlesOf(t *testing.T, zone *storage.Zone, session string) []string {
	t.Helper()
	var out []string
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for {
			record, err := reader.Next()
			if err != nil {
				break
			}
			if record.Label != "" && record.ID == "" && record.Call == "" && record.Tool == "" {
				out = append(out, record.Label)
			}
		}
		f.Close()
	}
	return out
}

// TestAPlainCallJoinIsLandedAgainWhenAProgramArrives.
//
// The record that opened a stream said auxiliary because a model call was
// all that had run under the tool. Landed records do not change, so when a
// program turns up under the same tool later, the link is landed once more
// without the flag. Assembly reads the later one as the program it is.
func TestAPlainCallJoinIsLandedAgainWhenAProgramArrives(t *testing.T) {
	_, zone, base := started(t, "")
	const trace = "41414141-4141-7141-8141-414141414140"
	const toolID = "41414141-4141-7141-8141-414141414141"
	const early = "41414141-4141-7141-8141-414141414142"
	const inner = "41414141-4141-7141-8141-414141414143"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-again"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100001000000Z" + toolID
	post := func(body string) {
		t.Helper()
		resp := send(t, base, "/runs/batch", "application/json", []byte(body), "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` +
		`"end_time":"2026-09-20T10:00:10Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"go"}]}},` +
		`{"id":"` + toolID + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + toolDotted + `","run_type":"tool","name":"delegate",` +
		`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:09Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"call_d_1","content":"done"}}},` +
		`{"id":"` + early + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100002000000Z` + early + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:02Z",` +
		`"end_time":"2026-09-20T10:00:02Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"quick"}}}]]}}]}`)
	collector := collectorOver(zone)
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	session := landed.Sessions[0]
	child := StreamName(toolID, "delegate")
	flagged, plain := linksTo(t, zone, session, child)
	if flagged == 0 || plain != 0 {
		t.Fatalf("after a plain call: %d auxiliary links, %d plain, want some and none", flagged, plain)
	}

	post(`{"post":[{"id":"` + inner + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100003000000Z` + inner + `",` +
		`"run_type":"chain","name":"analyst","start_time":"2026-09-20T10:00:03Z",` +
		`"end_time":"2026-09-20T10:00:08Z",` + owner + `}]}`)
	if _, err := collector.Collect(); err != nil {
		t.Fatal(err)
	}
	if _, plain := linksTo(t, zone, session, child); plain != 1 {
		t.Errorf("after the program arrived: %d plain links, want exactly one landed again", plain)
	}
}

// linksTo counts the records that open a stream, with and without the
// auxiliary flag.
func linksTo(t *testing.T, zone *storage.Zone, session, child string) (flagged, plain int) {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for {
			record, err := reader.Next()
			if err != nil {
				break
			}
			if record.Child != child {
				continue
			}
			aux := false
			for _, fl := range record.Flags {
				if fl == "auxiliary" {
					aux = true
				}
			}
			if aux {
				flagged++
			} else {
				plain++
			}
		}
		f.Close()
	}
	return flagged, plain
}

// TestARequestThatCannotLandWholeLandsNothing.
//
// A field can be valid JSON on its own and impossible to write inside a
// record: one nested to within a few levels of the depth limit gains three
// containers when it is placed in a part. The writer found that part way
// through a request, after the streams before it were already on disk. The
// request was then set aside, and those files were never indexed and their
// sessions never reported. Now every record is encoded to nowhere before any
// file is written, and a request that cannot land whole lands nothing.
//
// The field arrives as a multipart part of its own, which is how a client
// sends one and the only way it passes the receiver: validated alone it is
// under the limit, and the receiver reads it as valid JSON, which it is.
func TestARequestThatCannotLandWholeLandsNothing(t *testing.T) {
	_, zone, base := started(t, "")
	const trace = "81818181-8181-7181-8181-818181818180"
	const toolID = "81818181-8181-7181-8181-818181818181"
	const nested = "81818181-8181-7181-8181-818181818182"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-whole"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100001000000Z" + toolID
	deep := strings.Repeat("[", 9998) + strings.Repeat("]", 9998)

	// Two streams: main, and the tool's. The stream that sorts first lands
	// first, and the unencodable field is on a record in the one that sorts
	// second.
	parts := [][2]string{
		{"post." + trace, `{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
			`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` +
			`"end_time":"2026-09-20T10:00:10Z",` + owner + `}`},
		{"post." + trace + ".inputs", `{"messages":[{"role":"user","content":"go"}]}`},
		{"post." + trace + ".too_deep", deep},
		{"post." + toolID, `{"id":"` + toolID + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
			`"dotted_order":"` + toolDotted + `","run_type":"tool","name":"delegate",` +
			`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:09Z",` + owner + `}`},
		{"post." + toolID + ".outputs", `{"output":{"type":"tool","tool_call_id":"call_d","content":"ok"}}`},
		{"post." + nested, `{"id":"` + nested + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
			`"dotted_order":"` + toolDotted + `.20260920T100002000000Z` + nested + `",` +
			`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:02Z",` +
			`"end_time":"2026-09-20T10:00:03Z",` + owner + `}`},
		{"post." + nested + ".outputs", `{"generations":[[{"message":{"kwargs":{"content":"fine"}}}]]}`},
	}
	body, contentType := multipartOf(parts)
	resp := send(t, base, "/runs/multipart", contentType, body, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: every part is valid JSON and should be accepted", resp.StatusCode)
	}
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Unreadable != 1 {
		t.Errorf("%d unreadable, want 1", landed.Unreadable)
	}
	if landed.Files != 0 || len(landed.Sessions) != 0 {
		t.Errorf("%d files landed in %v from a request that could not land whole", landed.Files, landed.Sessions)
	}
	sessions, _ := os.ReadDir(zone.Root())
	for _, d := range sessions {
		if strings.HasPrefix(d.Name(), "ls-") {
			t.Errorf("a session directory %s exists with files nothing will index", d.Name())
		}
	}
}

// multipartOf builds a body the way the client does, one JSON part per name.
func multipartOf(parts [][2]string) ([]byte, string) {
	const boundary = "aszwhole"
	var b bytes.Buffer
	for _, part := range parts {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Disposition: form-data; name=\"" + part[0] + "\"\r\n")
		b.WriteString("Content-Type: application/json\r\n\r\n")
		b.WriteString(part[1])
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), "multipart/form-data; boundary=" + boundary
}

// TestARequestWithTwoSessionsLandsNeitherWhenOneCannotLand.
//
// One request can carry runs of two conversations. Checking each session as
// it was landed let the first land and then set the request aside on the
// second: the first's files were on disk and indexed, but the collector
// returned an error, so the session was never reported and never parsed
// that pass. Every session is checked before any is landed.
func TestARequestWithTwoSessionsLandsNeitherWhenOneCannotLand(t *testing.T) {
	_, zone, base := started(t, "")
	const good = "91919191-9191-7191-8191-919191919191"
	const bad = "91919191-9191-7191-8191-919191919192"
	deep := strings.Repeat("[", 9998) + strings.Repeat("]", 9998)
	run := func(id, thread string) string {
		return `{"id":"` + id + `","trace_id":"` + id + `","dotted_order":"20260920T100000000000Z` + id + `",` +
			`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` +
			`"end_time":"2026-09-20T10:00:01Z","session_name":"asz",` +
			`"extra":{"metadata":{"thread_id":"` + thread + `"}}}`
	}
	// Session "a" sorts before session "b", so "a" would have landed first.
	body, contentType := multipartOf([][2]string{
		{"post." + good, run(good, "a-fine")},
		{"post." + good + ".inputs", `{"messages":[{"role":"user","content":"hello"}]}`},
		{"post." + bad, run(bad, "b-deep")},
		{"post." + bad + ".inputs", `{"messages":[{"role":"user","content":"hello"}]}`},
		{"post." + bad + ".too_deep", deep},
	})
	resp := send(t, base, "/runs/multipart", contentType, body, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Unreadable != 1 || landed.Files != 0 || len(landed.Sessions) != 0 {
		t.Errorf("unreadable=%d files=%d sessions=%v; want the request set aside and nothing landed",
			landed.Unreadable, landed.Files, landed.Sessions)
	}
	entries, _ := os.ReadDir(zone.Root())
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), "ls-") {
			t.Errorf("session %s was created from a request that could not land whole", d.Name())
		}
	}
}
