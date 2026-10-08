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
	"net/http"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A decorated function and the model call it makes, as LangSmith JS sends
// them: the thread is on the function alone, since that client passes a
// decorated function's metadata to none of the runs inside it.
const (
	inheritRoot  = "14141414-1414-7414-8414-141414141410"
	inheritChild = "14141414-1414-7414-8414-141414141411"
	inheritOrder = "20260920T100000000000Z" + inheritRoot
)

func inheritFunction(thread string) string {
	return `{"id":"` + inheritRoot + `","trace_id":"` + inheritRoot + `","dotted_order":"` + inheritOrder + `",` +
		`"run_type":"chain","name":"review","start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:03Z",` +
		`"session_name":"asz","extra":{"metadata":{"thread_id":"` + thread + `"},"runtime":{"library":"langsmith"}},` +
		`"inputs":{"question":"what changed in the docs"},"outputs":{"answer":"two pages"}}`
}

func inheritCall(metadata string) string {
	return `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `","parent_run_id":"` + inheritRoot + `",` +
		`"dotted_order":"` + inheritOrder + `.20260920T100001000000Z` + inheritChild + `",` +
		`"run_type":"llm","name":"FakeListChatModel","start_time":"2026-09-20T10:00:01Z",` +
		`"end_time":"2026-09-20T10:00:02Z","session_name":"asz","extra":{"metadata":{` + metadata + `}},` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"the call's own answer"}}}]]}}`
}

func postBatch(t *testing.T, base, body string) {
	t.Helper()
	resp := send(t, base, "/runs/batch", "application/json", []byte(body), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestARunWithNoThreadTakesTheFunctionsItRanInside. The call names no thread,
// but the function it ran inside does, and the trace and the parent on the
// wire say the call ran there. Landed under its trace, the answer was a
// conversation of its own, and the question's conversation had none. The
// call is first in the batch, so the order inside a request decides nothing.
func TestARunWithNoThreadTakesTheFunctionsItRanInside(t *testing.T) {
	_, zone, base := started(t, "")
	postBatch(t, base, `{"post":[`+inheritCall(``)+`,`+inheritFunction("t-inherit")+`]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(landed.Sessions) != 1 || landed.Unassigned != 0 {
		t.Fatalf("sessions %v, %d unassigned, want the function's thread alone", landed.Sessions, landed.Unassigned)
	}
	if !mentions(t, zone, landed.Sessions[0], "the call's own answer") {
		t.Error("the call's answer is not in the function's conversation")
	}
}

// TestARunThatArrivesBeforeItsFunctionStillTakesItsThread. The client sends
// several batches at once and retries a failed one, so the call can arrive a
// pass before the function. It waits for its ancestry, and its conversation
// is decided when the function has arrived, not when it first came: the
// unassigned name it would have had then was remembered, and kept when the
// request was tried again.
func TestARunThatArrivesBeforeItsFunctionStillTakesItsThread(t *testing.T) {
	_, zone, base := started(t, "")
	collector := collectorOver(zone)
	postBatch(t, base, `{"post":[`+inheritCall(``)+`]}`)
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Waiting != 1 || landed.Records != 0 {
		t.Fatalf("%d waiting, %d records, want the call held for its function", landed.Waiting, landed.Records)
	}
	postBatch(t, base, `{"post":[`+inheritFunction("t-later")+`]}`)
	landed, err = collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(landed.Sessions) != 1 || landed.Unassigned != 0 {
		t.Fatalf("sessions %v, %d unassigned, want the function's thread alone", landed.Sessions, landed.Unassigned)
	}
	if !mentions(t, zone, landed.Sessions[0], "the call's own answer") {
		t.Error("the call's answer is not in the function's conversation")
	}
}

// TestARunsOwnThreadIsKept. A run that supplies a thread keeps it, whatever
// the run it ran inside supplied.
func TestARunsOwnThreadIsKept(t *testing.T) {
	_, zone, base := started(t, "")
	postBatch(t, base, `{"post":[`+inheritFunction("t-outer")+`,`+inheritCall(`"thread_id":"t-own"`)+`]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(landed.Sessions) != 2 {
		t.Fatalf("sessions %v, want the function's and the call's own", landed.Sessions)
	}
	for _, session := range landed.Sessions {
		answer := mentions(t, zone, session, "the call's own answer")
		if strings.Contains(session, "t-own") != answer {
			t.Errorf("%s: holds the call's answer %v", session, answer)
		}
	}
}

// TestATraceThatSuppliedNoThreadIsNotGivenOne. With no thread anywhere in the
// trace, nothing is inherited: the function and the call land together under
// the trace, in a session named as unassigned.
func TestATraceThatSuppliedNoThreadIsNotGivenOne(t *testing.T) {
	_, zone, base := started(t, "")
	function := strings.Replace(inheritFunction("x"), `"thread_id":"x"`, ``, 1)
	postBatch(t, base, `{"post":[`+function+`,`+inheritCall(``)+`]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(landed.Sessions) != 1 || landed.Sessions[0] != UnassignedID(inheritRoot) || landed.Unassigned != 2 {
		t.Fatalf("sessions %v, %d unassigned, want both runs under %s",
			landed.Sessions, landed.Unassigned, UnassignedID(inheritRoot))
	}
}

// TestARunLandedUnassignedStaysWhereItLanded. A call whose function never
// arrived while it waited is landed under its trace. When the function and
// an update to the call arrive later, the update stays with the call's first
// arrival: one run is never split across two conversations.
func TestARunLandedUnassignedStaysWhereItLanded(t *testing.T) {
	_, zone, base := started(t, "")
	collector := collectorOver(zone)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+start+`]}`)
	for pass := 0; pass <= deferLimit; pass++ {
		if _, err := collector.Collect(); err != nil {
			t.Fatal(err)
		}
	}
	if !mentions(t, zone, UnassignedID(inheritRoot), inheritChild) {
		t.Fatal("the call was not landed under its trace once it had waited as long as it may")
	}
	update := `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `",` +
		`"dotted_order":"` + inheritOrder + `.20260920T100001000000Z` + inheritChild + `",` +
		`"end_time":"2026-09-20T10:00:02Z",` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"the late answer"}}}]]}}`
	postBatch(t, base, `{"post":[`+inheritFunction("t-late")+`],"patch":[`+update+`]}`)
	if _, err := collector.Collect(); err != nil {
		t.Fatal(err)
	}
	if !mentions(t, zone, UnassignedID(inheritRoot), "the late answer") {
		t.Error("the call's update left the conversation its start landed in")
	}
}

// TestAnUnassignedRunAboveIsPassedOver. The step a call ran inside supplied
// no thread and landed unassigned, because the function above it did not
// arrive while it waited. The function supplied a thread when it came, and the
// call arriving with it takes that thread: the step's unassigned name was
// never supplied by anyone, so it is not inherited.
func TestAnUnassignedRunAboveIsPassedOver(t *testing.T) {
	_, zone, base := started(t, "")
	collector := collectorOver(zone)
	const step = "14141414-1414-7414-8414-141414141412"
	stepOrder := inheritOrder + ".20260920T100000500000Z" + step
	postBatch(t, base, `{"post":[{"id":"`+step+`","trace_id":"`+inheritRoot+`","parent_run_id":"`+inheritRoot+`",`+
		`"dotted_order":"`+stepOrder+`","run_type":"chain","name":"step","start_time":"2026-09-20T10:00:00.5Z",`+
		`"end_time":"2026-09-20T10:00:02.5Z","session_name":"asz","extra":{"metadata":{}},"outputs":{"done":true}}]}`)
	for pass := 0; pass <= deferLimit; pass++ {
		if _, err := collector.Collect(); err != nil {
			t.Fatal(err)
		}
	}
	if !mentions(t, zone, UnassignedID(inheritRoot), step) {
		t.Fatal("the step was not landed under its trace once it had waited as long as it may")
	}
	call := strings.Replace(inheritCall(``), `"parent_run_id":"`+inheritRoot+`","dotted_order":"`+inheritOrder,
		`"parent_run_id":"`+step+`","dotted_order":"`+stepOrder, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-deep")+`,`+call+`]}`)
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(landed.Sessions) != 1 || landed.Unassigned != 0 {
		t.Fatalf("sessions %v, %d unassigned, want the function's thread alone", landed.Sessions, landed.Unassigned)
	}
	if !mentions(t, zone, landed.Sessions[0], "the call's own answer") {
		t.Error("the call's answer is not in the function's conversation")
	}
}

// TestOneRunInOneRequestIsOneConversation. A batch carries the call's start,
// which names no thread, and an update to it that names one of its own. The
// run supplied a thread, so it keeps it, as it does when the two wait in
// requests of their own: both arrivals land in the call's own conversation,
// and the run is not split. The run is counted unassigned once at most, not
// once for each arrival.
func TestOneRunInOneRequestIsOneConversation(t *testing.T) {
	_, zone, base := started(t, "")
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	update := `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `",` +
		`"dotted_order":"` + inheritOrder + `.20260920T100001000000Z` + inheritChild + `",` +
		`"end_time":"2026-09-20T10:00:02Z","extra":{"metadata":{"thread_id":"t-other"}},` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"the patched answer"}}}]]}}`
	postBatch(t, base, `{"post":[`+inheritFunction("t-one")+`,`+start+`],"patch":[`+update+`]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	own := StorageID(Owner{Values: []string{"asz", "t-other"}, Thread: "t-other"})
	function := StorageID(Owner{Values: []string{"asz", "t-one"}, Thread: "t-one"})
	if !mentions(t, zone, own, `"FakeListChatModel"`) || !mentions(t, zone, own, "the patched answer") {
		t.Errorf("sessions %v: the call's start and its update are not both in its own conversation", landed.Sessions)
	}
	if mentions(t, zone, function, inheritChild) {
		t.Error("an arrival of the call landed in the function's conversation: the run is split")
	}

	// And with no thread anywhere, the run's two arrivals are one run unassigned.
	_, zone, base = started(t, "")
	keyless := strings.Replace(inheritFunction("x"), `"thread_id":"x"`, ``, 1)
	opened := strings.Replace(keyless, `"end_time":"2026-09-20T10:00:03Z",`, ``, 1)
	ended := `{"id":"` + inheritRoot + `","trace_id":"` + inheritRoot + `","dotted_order":"` + inheritOrder + `",` +
		`"end_time":"2026-09-20T10:00:03Z","outputs":{"answer":"two pages"}}`
	postBatch(t, base, `{"post":[`+opened+`],"patch":[`+ended+`]}`)
	landed, err = collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Unassigned != 1 {
		t.Errorf("%d unassigned, want 1: one run supplied no conversation", landed.Unassigned)
	}
}

// TestARunAboveThatArrivedWithNoKindIsStillRead. An update arrives for a run
// whose start never came, with no kind, and between the call and the function
// that supplied the thread. It supplied none, so it is passed over, as it is
// when it landed in an earlier request; it is not taken for a run that has not
// arrived, which would stop the walk.
func TestARunAboveThatArrivedWithNoKindIsStillRead(t *testing.T) {
	_, zone, base := started(t, "")
	const step = "14141414-1414-7414-8414-141414141413"
	stepOrder := inheritOrder + ".20260920T100000500000Z" + step
	update := `{"id":"` + step + `","trace_id":"` + inheritRoot + `","dotted_order":"` + stepOrder + `",` +
		`"end_time":"2026-09-20T10:00:02.5Z","outputs":{"done":true}}`
	call := strings.Replace(inheritCall(``), `"dotted_order":"`+inheritOrder, `"dotted_order":"`+stepOrder, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-kind")+`,`+call+`],"patch":[`+update+`]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Unassigned != 0 || !mentions(t, zone, StorageID(Owner{Values: []string{"asz", "t-kind"}, Thread: "t-kind"}), "the call's own answer") {
		t.Fatalf("sessions %v, %d unassigned, want the call in the function's conversation", landed.Sessions, landed.Unassigned)
	}
}

// TestARunThatSuppliedAThreadWithNoKindIsRemembered. The function's update,
// naming the thread but no kind, waits for the run it ran inside. The call
// below it, arriving first, is placed in the same pass once that run arrives,
// before the function's request is tried again. It finds the function's
// thread only if the waiting update was remembered with it.
func TestARunThatSuppliedAThreadWithNoKindIsRemembered(t *testing.T) {
	_, zone, base := started(t, "")
	const outer = "14141414-1414-7414-8414-141414141414"
	outerOrder := "20260920T095959000000Z" + outer
	functionOrder := outerOrder + ".20260920T100000000000Z" + inheritRoot
	callOrder := functionOrder + ".20260920T100001000000Z" + inheritChild
	call := strings.Replace(inheritCall(``), `"trace_id":"`+inheritRoot+`"`, `"trace_id":"`+outer+`"`, 1)
	call = strings.Replace(call, `"dotted_order":"`+inheritOrder+`.20260920T100001000000Z`+inheritChild, `"dotted_order":"`+callOrder, 1)
	postBatch(t, base, `{"post":[`+call+`]}`)
	postBatch(t, base, `{"patch":[{"id":"`+inheritRoot+`","trace_id":"`+outer+`","parent_run_id":"`+outer+`",`+
		`"dotted_order":"`+functionOrder+`","end_time":"2026-09-20T10:00:03Z","session_name":"asz",`+
		`"extra":{"metadata":{"thread_id":"t-remembered"}},"outputs":{"answer":"two pages"}}]}`)
	postBatch(t, base, `{"post":[{"id":"`+outer+`","trace_id":"`+outer+`","dotted_order":"`+outerOrder+`",`+
		`"run_type":"chain","name":"outer","start_time":"2026-09-20T09:59:59Z","end_time":"2026-09-20T10:00:04Z",`+
		`"session_name":"asz","extra":{"metadata":{}},"inputs":{"question":"what changed"}}]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	thread := StorageID(Owner{Values: []string{"asz", "t-remembered"}, Thread: "t-remembered"})
	if !mentions(t, zone, thread, "the call's own answer") {
		t.Errorf("sessions %v: the call is not in the conversation of the function it ran inside", landed.Sessions)
	}
}

// TestInheritingStopsAtARunNotKnown. The run between the call and the
// function was forgotten, or has not arrived. It may have supplied a thread of
// its own, so the function's further up is not taken over it. A run between
// that supplied nothing, by either unassigned name, is passed over.
func TestInheritingStopsAtARunNotKnown(t *testing.T) {
	const step = "14141414-1414-7414-8414-141414141415"
	call := Run{ID: inheritChild, TraceID: inheritRoot,
		Dotted: inheritOrder + ".20260920T100000500000Z" + step + ".20260920T100001000000Z" + inheritChild}
	memory := &pending{Runs: map[string]pendingRun{inheritRoot: {Trace: inheritRoot, Session: "ls-function"}}}
	if got := memory.inherited(call); got != "" {
		t.Errorf("took %q past a run that is not known", got)
	}
	for _, name := range []string{UnassignedID(inheritRoot), UnassignedID(step)} {
		memory.Runs[step] = pendingRun{Trace: inheritRoot, Session: name}
		if got := memory.inherited(call); got != "ls-function" {
			t.Errorf("a run between landed as %q: took %q, want the function's", name, got)
		}
	}
	memory.Runs[step] = pendingRun{Trace: inheritRoot, Session: "ls-step"}
	if got := memory.inherited(call); got != "ls-step" {
		t.Errorf("took %q, want the nearest supplied, the step's", got)
	}
}

// landInOrder sends each body as a request of its own, in order, and then
// collects them in one pass, as a client sending several batches at once
// leaves them in the inbox.
func landInOrder(t *testing.T, bodies ...string) *storage.Zone {
	t.Helper()
	_, zone, base := started(t, "")
	for _, body := range bodies {
		postBatch(t, base, body)
	}
	if _, err := collectorOver(zone).Collect(); err != nil {
		t.Fatal(err)
	}
	return zone
}

// TestTheOrderOfRequestsDoesNotDecide. The same runs, sent as requests in
// different orders and batched differently, land the same. A step with no
// kind and no thread sits between the call and the function: while its
// request waited it was not remembered, and the call, placed first, stopped
// at it as at a run that had not arrived. And a call's start that names no
// thread, with an update that names its own: the update decided when the two
// waited apart, and the start when they came together.
func TestTheOrderOfRequestsDoesNotDecide(t *testing.T) {
	const step = "14141414-1414-7414-8414-141414141416"
	stepOrder := inheritOrder + ".20260920T100000500000Z" + step
	function := `{"post":[` + inheritFunction("t-order") + `]}`
	thread := StorageID(Owner{Values: []string{"asz", "t-order"}, Thread: "t-order"})
	stepUpdate := `{"id":"` + step + `","trace_id":"` + inheritRoot + `","dotted_order":"` + stepOrder + `",` +
		`"end_time":"2026-09-20T10:00:02.5Z","outputs":{"done":true}}`
	call := strings.Replace(inheritCall(``), `"dotted_order":"`+inheritOrder, `"dotted_order":"`+stepOrder, 1)
	for name, bodies := range map[string][]string{
		"call, step, function":    {`{"post":[` + call + `]}`, `{"patch":[` + stepUpdate + `]}`, function},
		"step, call, function":    {`{"patch":[` + stepUpdate + `]}`, `{"post":[` + call + `]}`, function},
		"call and step, function": {`{"post":[` + call + `],"patch":[` + stepUpdate + `]}`, function},
	} {
		zone := landInOrder(t, bodies...)
		if !mentions(t, zone, thread, "the call's own answer") {
			t.Errorf("%s: the call is not in the function's conversation", name)
		}
	}

	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	update := `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `",` +
		`"dotted_order":"` + inheritOrder + `.20260920T100001000000Z` + inheritChild + `",` +
		`"end_time":"2026-09-20T10:00:02Z","extra":{"metadata":{"thread_id":"t-own"}},` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"the patched answer"}}}]]}}`
	own := StorageID(Owner{Values: []string{"asz", "t-own"}, Thread: "t-own"})
	for name, bodies := range map[string][]string{
		"start, update, function":    {`{"post":[` + start + `]}`, `{"patch":[` + update + `]}`, function},
		"start and update, function": {`{"post":[` + start + `],"patch":[` + update + `]}`, function},
	} {
		zone := landInOrder(t, bodies...)
		if !mentions(t, zone, own, `"FakeListChatModel"`) || !mentions(t, zone, own, "the patched answer") {
			t.Errorf("%s: the call's start and its update are not both in its own conversation", name)
		}
		if mentions(t, zone, thread, inheritChild) {
			t.Errorf("%s: an arrival of the call landed in the function's conversation", name)
		}
	}
}

// TestAThreadNamedBeforeItsProjectIsNotWhole. An update names the call's own
// thread but no project, since an update carries only what changed, and it
// can arrive before the call's start, which carries the project. Remembered as
// the call's conversation while it waited, it named the conversation with an
// empty project, and the start, carrying both, could no longer decide.
func TestAThreadNamedBeforeItsProjectIsNotWhole(t *testing.T) {
	order := inheritOrder + ".20260920T100001000000Z" + inheritChild
	update := `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `","dotted_order":"` + order + `",` +
		`"end_time":"2026-09-20T10:00:02Z","extra":{"metadata":{"thread_id":"t-own"}},` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"the patched answer"}}}]]}}`
	start := strings.Replace(inheritCall(`"thread_id":"t-own"`), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	own := StorageID(Owner{Values: []string{"asz", "t-own"}, Thread: "t-own"})
	outer := StorageID(Owner{Values: []string{"asz", "t-outer"}, Thread: "t-outer"})

	// In requests of their own, the update first.
	zone := landInOrder(t, `{"patch":[`+update+`]}`, `{"post":[`+start+`]}`, `{"post":[`+inheritFunction("t-outer")+`]}`)
	if !mentions(t, zone, own, `"FakeListChatModel"`) || !mentions(t, zone, own, "the patched answer") {
		t.Error("apart: the call's update and its start are not both in its own conversation, project and all")
	}

	// In one request, the update before the start.
	_, zone, base := started(t, "")
	// A multipart body carries the outputs as a part of their own.
	body, contentType := multipartOf([][2]string{
		{"patch." + inheritChild, strings.Replace(update,
			`,"outputs":{"generations":[[{"message":{"kwargs":{"content":"the patched answer"}}}]]}`, ``, 1)},
		{"patch." + inheritChild + ".outputs", `{"generations":[[{"message":{"kwargs":{"content":"the patched answer"}}}]]}`},
		{"post." + inheritChild, start},
		{"post." + inheritRoot, inheritFunction("t-outer")},
	})
	resp := send(t, base, "/runs/multipart", contentType, body, "")
	resp.Body.Close()
	if _, err := collectorOver(zone).Collect(); err != nil {
		t.Fatal(err)
	}
	// The update, read before the start, names no kind and lands as the
	// run's data, so where it landed is found by its record id.
	if !mentions(t, zone, own, `"FakeListChatModel"`) || !mentions(t, zone, own, inheritChild+":patch:") {
		t.Error("together: the call's update and its start are not both in its own conversation, project and all")
	}

	// The update waits alone, and the start that follows names no thread.
	// The update's thread is still the call's own: the call does not take
	// the function's in its place, and a run below it does not pass over it
	// to take the function's either.
	const below = "14141414-1414-7414-8414-141414141417"
	keyless := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	child := `{"id":"` + below + `","trace_id":"` + inheritRoot + `","parent_run_id":"` + inheritChild + `",` +
		`"dotted_order":"` + order + `.20260920T100001500000Z` + below + `",` +
		`"run_type":"tool","name":"lookup","start_time":"2026-09-20T10:00:01.5Z","end_time":"2026-09-20T10:00:01.8Z",` +
		`"session_name":"asz","extra":{"metadata":{}},"outputs":{"output":"the tool's own result"}}`
	_, zone, base = started(t, "")
	collector := collectorOver(zone)
	postBatch(t, base, `{"patch":[`+update+`]}`)
	if _, err := collector.Collect(); err != nil {
		t.Fatal(err)
	}
	postBatch(t, base, `{"post":[`+inheritFunction("t-outer")+`,`+keyless+`,`+child+`]}`)
	if _, err := collector.Collect(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`"FakeListChatModel"`, "the patched answer", "the tool's own result"} {
		if mentions(t, zone, outer, text) {
			t.Errorf("%s landed in the function's conversation, although the call named a thread of its own", text)
		}
	}
}
