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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// A run whose conversation cannot be told, because what would say it was
// forgotten or never came, is not collected once its request has waited as
// long as it may. These tests use the function and call of inherit_test.go,
// and three runs of their own.
const (
	lostStep  = "15151515-1515-7515-8515-151515151511"
	lostBelow = "15151515-1515-7515-8515-151515151512"
	lostOther = "15151515-1515-7515-8515-151515151513"
)

var lostCallOrder = inheritOrder + ".20260920T100001000000Z" + inheritChild

// lostClock is a collector's clock a test moves by hand, so what is
// remembered can be forgotten by age.
type lostClock struct{ at time.Time }

func (c *lostClock) now() time.Time {
	c.at = c.at.Add(time.Second)
	return c.at
}

func clockedOver(zone *storage.Zone) (*Collector, *lostClock) {
	clock := &lostClock{at: time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)}
	return &Collector{Zone: zone, Now: clock.now}, clock
}

// sparse is an update as update_run sends it: the id, the trace, the dotted
// order, the end and the outputs, and nothing that names a conversation. The
// outputs are a model call's, so a call's update keeps its text; a chain's
// is not kept, and a test finds it by its record id instead.
func sparse(id, trace, order, answer string) string {
	return `{"id":"` + id + `","trace_id":"` + trace + `","dotted_order":"` + order + `",` +
		`"end_time":"2026-09-20T10:00:02Z",` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"` + answer + `"}}}]]}}`
}

// updateOf is what a landed update of a run is named by.
func updateOf(id string) string { return id + ":patch:" }

// otherRun is a run of a trace of its own. A request holding it touches
// nothing a test reads, and its save forgets what is old enough.
func otherRun(thread string) string {
	order := "20260920T110000000000Z" + lostOther
	return `{"id":"` + lostOther + `","trace_id":"` + lostOther + `","dotted_order":"` + order + `",` +
		`"run_type":"chain","name":"other","start_time":"2026-09-20T11:00:00Z","end_time":"2026-09-20T11:00:01Z",` +
		`"session_name":"asz","extra":{"metadata":{"thread_id":"` + thread + `"},"runtime":{"library":"langsmith"}},` +
		`"outputs":{"answer":"elsewhere"}}`
}

// rootRun is a run at the root of a trace of its own, with the metadata
// given and the project when one is given. It is a traced function, whose
// outputs are kept, where a graph's own run keeps none.
func rootRun(id, project, metadata, answer string) string {
	named := ""
	if project != "" {
		named = `"session_name":"` + project + `",`
	}
	return `{"id":"` + id + `","trace_id":"` + id + `","dotted_order":"20260920T100000000000Z` + id + `",` +
		`"run_type":"chain","name":"root","start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:03Z",` +
		named + `"extra":{"metadata":{` + metadata + `},"runtime":{"library":"langsmith"}},"outputs":{"answer":"` + answer + `"}}`
}

func threadOf(name string) string {
	return StorageID(Owner{Values: []string{"asz", name}, Thread: name})
}

// passes collects n times and adds up what was dropped.
func passes(t *testing.T, c *Collector, n int) int {
	t.Helper()
	dropped := 0
	for i := 0; i < n; i++ {
		landed, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		dropped += landed.Dropped
	}
	return dropped
}

// TestASparseUpdateAfterItsStartWasForgottenIsNotCollected. The call's start
// landed. A day later what was remembered of it is forgotten, and an update
// that names no conversation can no longer be given back its own. It waits,
// in case its start comes again, and is then not collected.
func TestASparseUpdateAfterItsStartWasForgottenIsNotCollected(t *testing.T) {
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-lost")+`,`+start+`]}`)
	passes(t, c, 1)
	if !mentions(t, zone, threadOf("t-lost"), `"FakeListChatModel"`) {
		t.Fatal("the call's start did not land in the function's conversation")
	}
	clock.at = clock.at.Add(25 * time.Hour)
	postBatch(t, base, `{"post":[`+otherRun("t-other")+`]}`)
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "the forgotten end")+`]}`)
	if dropped := passes(t, c, deferLimit+1); dropped != 1 || landedAnywhere(t, zone, "the forgotten end") {
		t.Errorf("%d dropped: an update whose start was forgotten must not be collected", dropped)
	}
}

// TestACompleteUpdateLandsWithoutItsStart. The Python client's update
// carries the project and the thread, so it names its conversation itself.
// It lands in the first pass, with no start ever seen, and holds back no run
// of its request.
func TestACompleteUpdateLandsWithoutItsStart(t *testing.T) {
	_, zone, base := started(t, "")
	whole := rootRun(lostStep, "asz", `"thread_id":"t-whole"`, "a whole update")
	postBatch(t, base, `{"post":[`+otherRun("t-other")+`],"patch":[`+whole+`]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Waiting != 0 || landed.Dropped != 0 {
		t.Fatalf("%d waiting, %d dropped: a complete update needs no start", landed.Waiting, landed.Dropped)
	}
	if !mentions(t, zone, threadOf("t-whole"), "a whole update") || !mentions(t, zone, threadOf("t-other"), "elsewhere") {
		t.Error("the update or the run beside it is not in its conversation")
	}
}

// TestARunKeptByItsUpdatesIsRememberedPastADay. A run is remembered by when
// it was last seen, not by when it started. Updates that keep arriving keep
// it, and one 40 hours after its start still lands in its conversation.
func TestARunKeptByItsUpdatesIsRememberedPastADay(t *testing.T) {
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-kept")+`,`+start+`]}`)
	passes(t, c, 1)
	clock.at = clock.at.Add(20 * time.Hour)
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "still going")+`]}`)
	passes(t, c, 1)
	clock.at = clock.at.Add(20 * time.Hour)
	postBatch(t, base, `{"post":[`+otherRun("t-other")+`]}`)
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "done after 40 hours")+`]}`)
	if dropped := passes(t, c, 1); dropped != 0 {
		t.Fatalf("%d dropped: the call was seen within the day", dropped)
	}
	for _, text := range []string{"still going", "done after 40 hours"} {
		if !mentions(t, zone, threadOf("t-kept"), text) {
			t.Errorf("%q is not in the call's conversation", text)
		}
	}
}

// TestASparseUpdateWhoseStartNeverCameIsNotCollected. Updates of a run whose
// start never came, two in one request, one in a request of its own, and one
// after a restart. None of them proves a start, so none is collected, and the
// run stays remembered as one whose start was not seen.
func TestASparseUpdateWhoseStartNeverCameIsNotCollected(t *testing.T) {
	_, zone, base := started(t, "")
	order := "20260920T100000000000Z" + lostStep
	postBatch(t, base, `{"patch":[`+sparse(lostStep, lostStep, order, "first")+`,`+sparse(lostStep, lostStep, order, "second")+`]}`)
	postBatch(t, base, `{"patch":[`+sparse(lostStep, lostStep, order, "third")+`]}`)
	dropped := passes(t, collectorOver(zone), deferLimit+1)
	postBatch(t, base, `{"patch":[`+sparse(lostStep, lostStep, order, "after a restart")+`]}`)
	dropped += passes(t, collectorOver(zone), deferLimit+1)
	if dropped != 4 {
		t.Errorf("%d dropped, want all four updates", dropped)
	}
	if landedAnywhere(t, zone, lostStep) {
		t.Error("an update was collected, although the run's start never came")
	}
	open := loadPending(filepath.Join(zone.Root(), InboxDir, pendingFile))
	if known, ok := open.Runs[lostStep]; !ok || known.Create || known.Session != "" {
		t.Errorf("remembered %+v: the run must be remembered with no start seen and no conversation", known)
	}
}

// TestASparseUpdateBeforeItsStartInOneRequestLands. A request keeps the order
// the client wrote. Both clients write a run's start before its update, but a
// client written by hand can put the update first. The start is in the
// request either way, so the update is completed from it: it ends the model
// call, with the answer on the call. Completed only from the arrivals before
// it, the update had no kind, landed as a graph's own run, and its answer was
// dropped. Its response is landed after the request the start carries, so the
// response names the reply the request was answered with: landed first, it
// said the request was not there, and a response is never landed again.
func TestASparseUpdateBeforeItsStartInOneRequestLands(t *testing.T) {
	question := `{"lc":1,"type":"constructor","id":["langchain","schema","messages","HumanMessage"],` +
		`"kwargs":{"content":"what changed","type":"human","id":"m-question"}}`
	answer := `{"lc":1,"type":"constructor","id":["langchain","schema","messages","AIMessage"],` +
		`"kwargs":{"content":"ended first","type":"ai","id":"m-answer"}}`
	for _, order := range []string{"the update first", "the start first"} {
		_, zone, base := started(t, "")
		// The start carries what the model was asked and no end, so only the
		// update has the answer.
		start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
		start = strings.Replace(start, `"outputs":{"generations":[[{"message":{"kwargs":{"content":"the call's own answer"}}}]]}`,
			`"inputs":{"messages":[[`+question+`]]}`, 1)
		end := `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `","dotted_order":"` + lostCallOrder + `",` +
			`"end_time":"2026-09-20T10:00:02Z","outputs":{"generations":[[{"message":` + answer + `}]]}}`
		update := [2]string{"patch." + inheritChild, end}
		post := [2]string{"post." + inheritChild, start}
		parts := [][2]string{update, post, {"post." + inheritRoot, inheritFunction("t-together")}}
		if order == "the start first" {
			parts[0], parts[1] = post, update
		}
		body, contentType := multipartOf(parts)
		resp := send(t, base, "/runs/multipart", contentType, body, "")
		resp.Body.Close()
		c := collectorOver(zone)
		c.ProviderBodies = true
		landed, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		session := threadOf("t-together")
		if landed.Dropped != 0 || !mentions(t, zone, session, `"FakeListChatModel"`) {
			t.Errorf("%s: %d dropped: the start is not in the function's conversation", order, landed.Dropped)
		}
		if call, finished, found := answerOf(t, zone, session, "ended first"); call != inheritChild || !finished {
			t.Errorf("%s: the answer landed %v, on call %q, finished %v; want it on the finished call %s",
				order, found, call, finished, inheritChild)
		}
		if landed.Bodies != 2 {
			t.Errorf("%s: %d provider bodies landed, want the call's request and its response", order, landed.Bodies)
		}
		held, _, err := c.loadHeld(session)
		if err != nil {
			t.Fatal(err)
		}
		request, asked := held.Manifest(inheritChild + ":request")
		response, answered := held.Manifest(inheritChild + ":response")
		switch {
		case !asked || request.Messages == nil || request.Messages.Unavailable != "" || len(request.Messages.Entries) != 1:
			t.Errorf("%s: the request's history is %+v, want its one message", order, request.Messages)
		case !answered || response.Reply == nil || response.Reply.Unavailable != "" || response.Reply.Digest == "":
			t.Errorf("%s: the response's reply is %+v, want the answer it was given", order, response.Reply)
		}
		if calls := nodesOf(parsedTwice(t, c, session), model.KindLLMCall); calls != 1 {
			t.Errorf("%s: %d model calls in the conversation, want 1", order, calls)
		}
	}
}

// TestAFailedToolIsJoinedTheSameWhicheverArrivalOfTheCallComesFirst. A model
// call is still running when a tool of the name it later asks for fails, so
// the failure is not the answer to that call. When was the call made is read
// from every arrival of it in the request together. Taken from the last
// arrival alone, an update written before its start left the start, which
// has no end, and the call was read as made when it began, before the tool
// failed, and the failure was joined to it.
func TestAFailedToolIsJoinedTheSameWhicheverArrivalOfTheCallComesFirst(t *testing.T) {
	const tool = "18181818-1818-7818-8818-181818181812"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-failed"}}`
	start := `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `","parent_run_id":"` + inheritRoot + `",` +
		`"dotted_order":"` + lostCallOrder + `","run_type":"llm","name":"FakeListChatModel",` +
		`"start_time":"2026-09-20T10:00:01Z",` + owner + `}`
	end := `{"id":"` + inheritChild + `","trace_id":"` + inheritRoot + `","dotted_order":"` + lostCallOrder + `",` +
		`"end_time":"2026-09-20T10:00:05Z","outputs":{"generations":[[{"message":{"kwargs":` +
		`{"tool_calls":[{"id":"call_later","name":"lookup","args":{}}]}}}]]}}`
	failed := `{"id":"` + tool + `","trace_id":"` + inheritRoot + `","parent_run_id":"` + inheritRoot + `",` +
		`"dotted_order":"` + inheritOrder + `.20260920T100003000000Z` + tool + `","run_type":"tool","name":"lookup",` +
		`"start_time":"2026-09-20T10:00:03Z","end_time":"2026-09-20T10:00:04Z",` + owner + `,` +
		`"outputs":{},"error":"it raised"}`
	for _, order := range []string{"the update first", "the start first"} {
		_, zone, base := started(t, "")
		update := [2]string{"patch." + inheritChild, end}
		post := [2]string{"post." + inheritChild, start}
		parts := [][2]string{update, post, {"post." + tool, failed}, {"post." + inheritRoot, inheritFunction("t-failed")}}
		if order == "the start first" {
			parts[0], parts[1] = post, update
		}
		body, contentType := multipartOf(parts)
		resp := send(t, base, "/runs/multipart", contentType, body, "")
		resp.Body.Close()
		c := collectorOver(zone)
		if _, err := c.Collect(); err != nil {
			t.Fatal(err)
		}
		session := threadOf("t-failed")
		found := false
		for _, r := range landedRecords(t, zone, session) {
			if r.ID == tool {
				found = true
				if r.Tool != "" {
					t.Errorf("%s: the failure was joined to %q, which the model asked for after the tool failed", order, r.Tool)
				}
			}
		}
		if !found {
			t.Errorf("%s: the failed tool did not land", order)
		}
		parsedTwice(t, c, session)
	}
}

// TestASparseUpdateOvertakingItsStartWaitsForIt. The client sends from
// several threads at once, so the request with the update is published
// before the one with the start. The update waits, and lands in the same
// pass once the start has.
func TestASparseUpdateOvertakingItsStartWaitsForIt(t *testing.T) {
	_, zone, base := started(t, "")
	postBatch(t, base, `{"post":[`+inheritFunction("t-overtaken")+`]}`)
	passes(t, collectorOver(zone), 1)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "overtook its start")+`]}`)
	postBatch(t, base, `{"post":[`+start+`]}`)
	landed, err := collectorOver(zone).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Dropped != 0 || landed.Waiting != 0 || !mentions(t, zone, threadOf("t-overtaken"), "overtook its start") {
		t.Errorf("%d dropped, %d waiting: the update must land once its start has", landed.Dropped, landed.Waiting)
	}
}

// TestAStartPastTheWaitDropsOnlyItsUpdate. The start comes after its update
// has waited as long as it may. The update is not collected, and the start,
// which names its own thread, lands there: the conversation the update fell
// back to was never remembered as the run's.
func TestAStartPastTheWaitDropsOnlyItsUpdate(t *testing.T) {
	_, zone, base := started(t, "")
	order := "20260920T100000000000Z" + lostStep
	postBatch(t, base, `{"patch":[`+sparse(lostStep, lostStep, order, "too early")+`]}`)
	c := collectorOver(zone)
	if dropped := passes(t, c, deferLimit+1); dropped != 1 {
		t.Fatalf("%d dropped, want the update", dropped)
	}
	postBatch(t, base, `{"post":[`+rootRun(lostStep, "asz", `"thread_id":"t-late-start"`, "the start's own")+`]}`)
	passes(t, c, 1)
	if !mentions(t, zone, threadOf("t-late-start"), "the start's own") {
		t.Error("the start is not in its own conversation")
	}
	if landedAnywhere(t, zone, updateOf(lostStep)) || landedAnywhere(t, zone, UnassignedID(lostStep)) {
		t.Error("the update was collected, or the run landed unassigned")
	}
}

// TestARunUnderARunKnownOnlyFromAnUpdateIsNotCollected. The function names a
// live thread. The step inside it named a thread of its own, and was
// forgotten while the function was not. An update of the step then arrives,
// with no thread or with a thread but no project, and is not collected. A run
// inside the step that names no thread cannot take the function's past it:
// the step's start would say, and it is gone. It is not collected, after a
// restart too.
func TestARunUnderARunKnownOnlyFromAnUpdateIsNotCollected(t *testing.T) {
	stepOrder := inheritOrder + ".20260920T100000500000Z" + lostStep
	belowOrder := stepOrder + ".20260920T100001000000Z" + lostBelow
	step := `{"id":"` + lostStep + `","trace_id":"` + inheritRoot + `","parent_run_id":"` + inheritRoot + `",` +
		`"dotted_order":"` + stepOrder + `","run_type":"chain","name":"step","start_time":"2026-09-20T10:00:00.5Z",` +
		`"session_name":"asz","extra":{"metadata":{"thread_id":"t-step"}},"inputs":{"q":"the step's own"}}`
	below := `{"id":"` + lostBelow + `","trace_id":"` + inheritRoot + `","parent_run_id":"` + lostStep + `",` +
		`"dotted_order":"` + belowOrder + `","run_type":"chain","name":"below","start_time":"2026-09-20T10:00:01Z",` +
		`"end_time":"2026-09-20T10:00:01.5Z","session_name":"asz","extra":{"metadata":{}},"outputs":{"answer":"below the step"}}`
	for name, update := range map[string]string{
		"no thread": sparse(lostStep, inheritRoot, stepOrder, "the step's end"),
		"a thread with no project": `{"id":"` + lostStep + `","trace_id":"` + inheritRoot + `","dotted_order":"` + stepOrder + `",` +
			`"end_time":"2026-09-20T10:00:02.5Z","extra":{"metadata":{"thread_id":"t-step"}},"outputs":{"answer":"the step's end"}}`,
	} {
		_, zone, base := started(t, "")
		c, clock := clockedOver(zone)
		postBatch(t, base, `{"post":[`+inheritFunction("t-function")+`,`+step+`]}`)
		passes(t, c, 1)
		// The function is seen again, and the step is not.
		clock.at = clock.at.Add(20 * time.Hour)
		postBatch(t, base, `{"patch":[`+sparse(inheritRoot, inheritRoot, inheritOrder, "the function goes on")+`]}`)
		passes(t, c, 1)
		clock.at = clock.at.Add(10 * time.Hour)
		postBatch(t, base, `{"post":[`+otherRun("t-other")+`]}`)
		postBatch(t, base, `{"patch":[`+update+`]}`)
		if dropped := passes(t, c, deferLimit+1); dropped != 1 {
			t.Fatalf("%s: %d dropped, want the step's update", name, dropped)
		}
		restarted := &Collector{Zone: zone, Now: clock.now}
		postBatch(t, base, `{"post":[`+below+`]}`)
		if dropped := passes(t, restarted, deferLimit+1); dropped != 1 {
			t.Errorf("%s: %d dropped, want the run below the step", name, dropped)
		}
		if landedAnywhere(t, zone, lostBelow) || landedAnywhere(t, zone, updateOf(lostStep)) {
			t.Errorf("%s: a run whose conversation cannot be told was collected", name)
		}
		if !mentions(t, zone, threadOf("t-function"), updateOf(inheritRoot)) {
			t.Errorf("%s: the function's own update is not in its conversation", name)
		}
	}
}

// TestAStartWithNoThreadUnderAForgottenRunIsNotCollected. The function was
// forgotten by age. A call inside it that names no thread arrives, and what
// the function named is not known: the call is not collected.
func TestAStartWithNoThreadUnderAForgottenRunIsNotCollected(t *testing.T) {
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	postBatch(t, base, `{"post":[`+inheritFunction("t-gone")+`]}`)
	passes(t, c, 1)
	clock.at = clock.at.Add(25 * time.Hour)
	postBatch(t, base, `{"post":[`+otherRun("t-other")+`]}`)
	postBatch(t, base, `{"post":[`+inheritCall(``)+`]}`)
	if dropped := passes(t, c, deferLimit+1); dropped != 1 || landedAnywhere(t, zone, "the call's own answer") {
		t.Errorf("%d dropped: a call under a forgotten function must not be collected", dropped)
	}
}

// TestAnUnassignedRunKeepsItsConversation. A run with no thread anywhere
// above it lands in its own conversation, unassigned. A later update that
// carries a thread does not move it: one run is one conversation.
func TestAnUnassignedRunKeepsItsConversation(t *testing.T) {
	_, zone, base := started(t, "")
	c := collectorOver(zone)
	postBatch(t, base, `{"post":[`+rootRun(lostStep, "asz", ``, "no thread anywhere")+`]}`)
	passes(t, c, 1)
	postBatch(t, base, `{"patch":[`+rootRun(lostStep, "asz", `"thread_id":"t-later"`, "a thread too late")+`]}`)
	passes(t, c, 1)
	if !mentions(t, zone, UnassignedID(lostStep), "a thread too late") || landedAnywhere(t, zone, threadOf("t-later")) {
		t.Error("the update left the conversation its run landed in")
	}
}

// TestAnUpdateIsCompleteOnlyWithEveryDimension. Which dimensions name a
// conversation is configured. With the thread alone, an update that carries
// its thread names its conversation and lands. With a tenant too, an update
// that carries the thread and no tenant does not, and is not collected once
// it has waited; a start that leaves the tenant out supplies it empty.
func TestAnUpdateIsCompleteOnlyWithEveryDimension(t *testing.T) {
	keys := DefaultOwnership().Keys
	_, zone, base := started(t, "")
	c := collectorOver(zone)
	c.Ownership = Ownership{Keys: keys, Scope: []string{"thread"}}
	postBatch(t, base, `{"patch":[`+rootRun(lostStep, "", `"thread_id":"t-alone"`, "thread alone")+`]}`)
	if landed, err := c.Collect(); err != nil || landed.Waiting != 0 {
		t.Fatalf("%v, %d waiting: an update with every dimension needs no start", err, landed.Waiting)
	}
	if !mentions(t, zone, StorageID(Owner{Values: []string{"t-alone"}, Thread: "t-alone"}), "thread alone") {
		t.Error("the update is not in the conversation its thread names")
	}

	_, zone, base = started(t, "")
	c = collectorOver(zone)
	c.Ownership = Ownership{Keys: keys, Scope: []string{"tenant", "thread"}}
	postBatch(t, base, `{"patch":[`+rootRun(lostStep, "asz", `"thread_id":"t-tenant"`, "no tenant")+`]}`)
	postBatch(t, base, `{"post":[`+rootRun(lostBelow, "asz", `"thread_id":"t-start"`, "a start with no tenant")+`]}`)
	if dropped := passes(t, c, deferLimit+1); dropped != 1 || landedAnywhere(t, zone, lostStep) {
		t.Errorf("%d dropped: an update with no tenant must not be collected", dropped)
	}
	if !mentions(t, zone, StorageID(Owner{Values: []string{"", "t-start"}, Thread: "t-start"}), "a start with no tenant") {
		t.Error("the start that left the tenant out is not in its conversation")
	}
}

// inboxEmpty reports whether every request was taken out of the inbox, so a
// request is known to be done with rather than still waiting.
func inboxEmpty(t *testing.T, zone *storage.Zone) bool {
	t.Helper()
	waiting, err := NewInbox(zone).List()
	if err != nil {
		t.Fatal(err)
	}
	return len(waiting) == 0
}

// TestARunForgottenByCountIsNotKnown. More than 50,000 runs are remembered,
// and the oldest go, whatever their age. The call's update and a new call
// under the forgotten function that names no thread are then not collected.
func TestARunForgottenByCountIsNotKnown(t *testing.T) {
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-counted")+`,`+start+`]}`)
	passes(t, c, 1)
	path := filepath.Join(zone.Root(), InboxDir, pendingFile)
	open := loadPending(path)
	later := clock.now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < pendingLimit; i++ {
		open.Runs["newer-"+strconv.Itoa(i)] = pendingRun{Trace: "newer", Create: true, At: later}
	}
	if err := open.save(path, clock.now()); err != nil {
		t.Fatal(err)
	}
	if kept := loadPending(path); len(kept.Runs) != pendingLimit {
		t.Fatalf("%d remembered, want the limit", len(kept.Runs))
	} else if _, ok := kept.Runs[inheritChild]; ok {
		t.Fatal("the call is still remembered, although it is the oldest past the limit")
	}
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "counted out")+`]}`)
	const another = "15151515-1515-7515-8515-151515151514"
	newCall := strings.ReplaceAll(inheritCall(``), inheritChild, another)
	postBatch(t, base, `{"post":[`+newCall+`]}`)
	if dropped := passes(t, c, deferLimit+1); dropped != 2 || !inboxEmpty(t, zone) {
		t.Errorf("%d dropped: the update and the new call must not be collected", dropped)
	}
	if landedAnywhere(t, zone, "counted out") || landedAnywhere(t, zone, another) {
		t.Error("a run under a function forgotten by count was collected")
	}
}

// TestALaterStartDecidesForItsWaitingUpdate. A call's update, naming no
// conversation, comes before its start, under a function that is
// remembered. It is not given the function's conversation while it waits:
// the call's start names a thread of its own, and both land there, with or
// without a restart between.
func TestALaterStartDecidesForItsWaitingUpdate(t *testing.T) {
	for _, restart := range []bool{false, true} {
		_, zone, base := started(t, "")
		c := collectorOver(zone)
		postBatch(t, base, `{"post":[`+inheritFunction("t-function")+`]}`)
		passes(t, c, 1)
		postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "the call's own end")+`]}`)
		if landed, err := c.Collect(); err != nil || landed.Waiting != 1 {
			t.Fatalf("restart %v: %v, %d waiting: the update must wait for its start", restart, err, landed.Waiting)
		}
		if restart {
			c = collectorOver(zone)
		}
		start := strings.Replace(inheritCall(`"thread_id":"t-own"`), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
		postBatch(t, base, `{"post":[`+start+`]}`)
		if dropped := passes(t, c, 1); dropped != 0 || !inboxEmpty(t, zone) {
			t.Fatalf("restart %v: %d dropped: both must land once the start has", restart, dropped)
		}
		if !mentions(t, zone, threadOf("t-own"), "the call's own end") || mentions(t, zone, threadOf("t-function"), inheritChild) {
			t.Errorf("restart %v: the update is not with its start in the call's own conversation", restart)
		}
	}
}

// TestAnUnassignedRunBelowAForgottenRootStays. A trace with no thread
// anywhere lands unassigned, its root and the call inside it. The root is
// then forgotten, and the call is not. A later update of the call stays in
// the call's conversation, and a new run inside the call that names no
// thread is not collected: the root that would say it supplied none is gone.
func TestAnUnassignedRunBelowAForgottenRootStays(t *testing.T) {
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	function := strings.Replace(inheritFunction("x"), `"thread_id":"x"`, ``, 1)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+function+`,`+start+`]}`)
	passes(t, c, 1)
	if !mentions(t, zone, UnassignedID(inheritRoot), inheritChild) {
		t.Fatal("the call did not land unassigned with its root")
	}
	clock.at = clock.at.Add(20 * time.Hour)
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "still unassigned")+`]}`)
	passes(t, c, 1)
	clock.at = clock.at.Add(10 * time.Hour)
	postBatch(t, base, `{"post":[`+otherRun("t-other")+`]}`)
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "after the root went")+`]}`)
	belowOrder := lostCallOrder + ".20260920T100001500000Z" + lostBelow
	below := `{"id":"` + lostBelow + `","trace_id":"` + inheritRoot + `","parent_run_id":"` + inheritChild + `",` +
		`"dotted_order":"` + belowOrder + `","run_type":"tool","name":"lookup","start_time":"2026-09-20T10:00:01.5Z",` +
		`"end_time":"2026-09-20T10:00:01.8Z","session_name":"asz","extra":{"metadata":{}},"outputs":{"output":"below the call"}}`
	postBatch(t, base, `{"post":[`+below+`]}`)
	if dropped := passes(t, c, deferLimit+1); dropped != 1 || !inboxEmpty(t, zone) {
		t.Errorf("%d dropped, want the run below the call alone", dropped)
	}
	for _, text := range []string{"still unassigned", "after the root went"} {
		if !mentions(t, zone, UnassignedID(inheritRoot), text) {
			t.Errorf("%q is not in the call's unassigned conversation", text)
		}
	}
	if landedAnywhere(t, zone, "below the call") {
		t.Error("the run below the call was collected, although the root above it is gone")
	}
}

// TestAThreadWithNoTenantIsPartialOnEveryArrival. With a tenant in the
// configured key, a run's first arrival names nothing and its second, in
// the same request, names the project and a thread and no tenant. The second decides for
// the first, and is not whole, so neither lands. An update that carries the
// tenant and the thread lands at once.
func TestAThreadWithNoTenantIsPartialOnEveryArrival(t *testing.T) {
	_, zone, base := started(t, "")
	c := collectorOver(zone)
	c.Ownership = Ownership{Keys: DefaultOwnership().Keys, Scope: []string{"tenant", "thread"}}
	order := "20260920T100000000000Z" + lostStep
	named := `{"id":"` + lostStep + `","trace_id":"` + lostStep + `","dotted_order":"` + order + `",` +
		`"end_time":"2026-09-20T10:00:03Z","session_name":"asz","extra":{"metadata":{"thread_id":"t-tenant"}},` +
		`"outputs":{"answer":"no tenant"}}`
	postBatch(t, base, `{"patch":[`+sparse(lostStep, lostStep, order, "nothing named")+`,`+named+`]}`)
	if dropped := passes(t, c, deferLimit+1); dropped != 2 || landedAnywhere(t, zone, lostStep) {
		t.Errorf("%d dropped: both arrivals of a run with no tenant must not be collected", dropped)
	}
	whole := rootRun(lostBelow, "asz", `"thread_id":"t-whole","tenant":"acme"`, "tenant and thread")
	postBatch(t, base, `{"patch":[`+whole+`]}`)
	if landed, err := c.Collect(); err != nil || landed.Waiting != 0 || landed.Dropped != 0 {
		t.Fatalf("%v: %+v: an update with the whole key needs no start", err, landed)
	}
	if !mentions(t, zone, StorageID(Owner{Values: []string{"acme", "t-whole"}, Thread: "t-whole"}), "tenant and thread") {
		t.Error("the whole update is not in its conversation")
	}
}

// TestAStartTimeIsNotAStart. An update that names no conversation and
// carries the run's start time is still an update: only a run's start
// proves one was seen, so it is not collected.
func TestAStartTimeIsNotAStart(t *testing.T) {
	_, zone, base := started(t, "")
	order := "20260920T100000000000Z" + lostStep
	update := `{"id":"` + lostStep + `","trace_id":"` + lostStep + `","dotted_order":"` + order + `",` +
		`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:03Z","outputs":{"answer":"timed"}}`
	postBatch(t, base, `{"patch":[`+update+`]}`)
	if dropped := passes(t, collectorOver(zone), deferLimit+1); dropped != 1 || landedAnywhere(t, zone, lostStep) {
		t.Errorf("%d dropped: an update with a start time is not a start", dropped)
	}
	open := loadPending(filepath.Join(zone.Root(), InboxDir, pendingFile))
	if open.Runs[lostStep].Create {
		t.Error("a start time was remembered as a start")
	}
}

// TestAPartialStartUnderAThreadIsNotCollected. A call's start names a thread
// but carries no project, under a function that names its own. The call does
// not take the function's in place of its own, and its own is never whole,
// so once it has waited as long as it may it is not collected.
func TestAPartialStartUnderAThreadIsNotCollected(t *testing.T) {
	_, zone, base := started(t, "")
	c := collectorOver(zone)
	postBatch(t, base, `{"post":[`+inheritFunction("t-function")+`]}`)
	passes(t, c, 1)
	call := strings.Replace(inheritCall(`"thread_id":"t-own"`), `"session_name":"asz",`, ``, 1)
	postBatch(t, base, `{"post":[`+call+`]}`)
	if dropped := passes(t, c, deferLimit+1); dropped != 1 || !inboxEmpty(t, zone) {
		t.Errorf("%d dropped: a start whose own thread is never whole must not be collected", dropped)
	}
	if landedAnywhere(t, zone, "the call's own answer") {
		t.Error("the partial call was collected")
	}
}

// TestALongTraceRemembersItsAncestryNotEveryRun. A trace that runs for more
// than a day makes a tool run every hour. Each arrival kept the whole trace
// fresh, so the trace remembered every run it ever made, past the day and the
// count that bound what is remembered, and saved them all with every request.
// Now a trace forgets a run as the run itself is forgotten, and keeps the runs
// a remembered run ran inside: the root is kept, though it was last seen more
// than a day ago, and a run arriving under it still lands at once.
func TestALongTraceRemembersItsAncestryNotEveryRun(t *testing.T) {
	const root = "17171717-1717-7717-8717-100000000000"
	rootOrder := "20260920T100000000000Z" + root
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-long"}}`
	start := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	tool := func(hour int) (string, string) {
		id := fmt.Sprintf("17171717-1717-7717-8717-%012d", hour)
		at := start.Add(time.Duration(hour) * time.Hour)
		return id, `{"id":"` + id + `","trace_id":"` + root + `","parent_run_id":"` + root + `",` +
			`"dotted_order":"` + rootOrder + `.` + at.Format("20060102T150405000000Z") + id + `",` +
			`"run_type":"tool","name":"look","start_time":"` + at.Format(time.RFC3339) + `",` +
			`"end_time":"` + at.Add(time.Second).Format(time.RFC3339) + `",` + owner + `,` +
			`"outputs":{"output":"looked at hour ` + strconv.Itoa(hour) + `"}}`
	}
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	clock.at = start
	postBatch(t, base, `{"post":[{"id":"`+root+`","trace_id":"`+root+`","dotted_order":"`+rootOrder+`",`+
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",`+owner+`,`+
		`"inputs":{"messages":[{"role":"user","content":"keep looking"}]}}]}`)
	passes(t, c, 1)
	const hours = 30
	tools := map[int]string{}
	for hour := 1; hour <= hours; hour++ {
		clock.at = start.Add(time.Duration(hour) * time.Hour)
		id, run := tool(hour)
		tools[hour] = id
		postBatch(t, base, `{"post":[`+run+`]}`)
		if dropped := passes(t, c, 1); dropped != 0 {
			t.Fatalf("hour %d: %d dropped", hour, dropped)
		}
	}
	memory := loadPending(filepath.Join(zone.Root(), InboxDir, pendingFile)).Traces[root]
	if memory == nil {
		t.Fatal("the trace is not remembered, although a run of it arrived an hour ago")
	}
	if _, kept := memory.Runs[root]; !kept {
		t.Error("the root was forgotten, although the runs inside it are remembered")
	}
	for hour := 1; hour <= hours; hour++ {
		_, run := memory.Runs[tools[hour]]
		_, named := memory.ToolNames[tools[hour]]
		// A day before the last pass, give or take the seconds a pass takes.
		switch {
		case hour < hours-24 && (run || named):
			t.Errorf("the tool run of hour %d is remembered after a day", hour)
		case hour > hours-24 && (!run || !named):
			t.Errorf("the tool run of hour %d was forgotten within the day", hour)
		}
	}
	if len(memory.Runs) > 26 {
		t.Errorf("the trace remembers %d runs, want the root and a day of runs", len(memory.Runs))
	}

	clock.at = start.Add((hours + 1) * time.Hour)
	_, run := tool(hours + 1)
	postBatch(t, base, `{"post":[`+run+`]}`)
	landed, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	session := threadOf("t-long")
	if landed.Waiting != 0 || landed.Dropped != 0 || !mentions(t, zone, session, "looked at hour 31") {
		t.Errorf("%d waiting, %d dropped: a run inside the remembered root must land at once", landed.Waiting, landed.Dropped)
	}

	// A run inside the first tool run, forgotten a day ago, waits for it as a
	// run of a trace idle for a day does, and then lands.
	const below = "17171717-1717-7717-8717-200000000000"
	_, first := tool(1)
	var firstOrder struct {
		Dotted string `json:"dotted_order"`
	}
	if err := json.Unmarshal([]byte(first), &firstOrder); err != nil {
		t.Fatal(err)
	}
	postBatch(t, base, `{"post":[{"id":"`+below+`","trace_id":"`+root+`","parent_run_id":"`+tools[1]+`",`+
		`"dotted_order":"`+firstOrder.Dotted+`.20260920T110000500000Z`+below+`","run_type":"chain","name":"step",`+
		`"start_time":"2026-09-20T11:00:00.5Z","end_time":"2026-09-20T11:00:00.8Z",`+owner+`,"outputs":{"answer":"late below"}}]}`)
	waited, placed := 0, 0
	for i := 0; i <= deferLimit; i++ {
		landed, err := c.Collect()
		if err != nil {
			t.Fatal(err)
		}
		waited, placed = waited+landed.Waiting, placed+landed.Placed
	}
	if waited != deferLimit || placed != 1 || !mentions(t, zone, session, below) {
		t.Errorf("%d waiting, %d placed: the run inside a forgotten run must wait for it and then land", waited, placed)
	}
	parsedTwice(t, c, session)
}

// TestATraceForgetsByCountWhatItsRunsForget. Past 50,000 remembered runs the
// oldest go, and the trace forgets them too, except a run a remembered run
// ran inside, however far above it is: a leaf two levels down keeps its
// parent and the root, and an old tool beside them goes with its name.
func TestATraceForgetsByCountWhatItsRunsForget(t *testing.T) {
	const (
		root = "19191919-1919-7919-8919-191919191910"
		mid  = "19191919-1919-7919-8919-191919191911"
		leaf = "19191919-1919-7919-8919-191919191912"
		old  = "19191919-1919-7919-8919-191919191913"
	)
	order := "20260920T100000000000Z" + root
	midOrder := order + ".20260920T100001000000Z" + mid
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) string {
		return now.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)
	}
	open := loadPending(filepath.Join(t.TempDir(), pendingFile))
	open.Runs[root] = pendingRun{Trace: root, Dotted: order, Create: true, At: at(-60)}
	open.Runs[mid] = pendingRun{Trace: root, Dotted: midOrder, Create: true, At: at(-60)}
	open.Runs[old] = pendingRun{Trace: root, Dotted: order + ".20260920T100002000000Z" + old, Create: true, At: at(-60)}
	open.Runs[leaf] = pendingRun{Trace: root, Dotted: midOrder + ".20260920T100003000000Z" + leaf, Create: true, At: at(-30)}
	for i := 0; i < pendingLimit-1; i++ {
		open.Runs["newer-"+strconv.Itoa(i)] = pendingRun{Trace: "newer", Create: true, At: at(-1)}
	}
	open.Traces[root] = &traceMemory{Runs: map[string]bool{root: false, mid: false, leaf: false, old: true},
		ToolNames: map[string]string{old: "lookup"}, Root: true, At: at(-1)}
	open.evict(now)
	memory := open.Traces[root]
	if len(open.Runs) != pendingLimit || memory == nil {
		t.Fatalf("%d runs remembered, trace %v: want the limit, and the trace", len(open.Runs), memory)
	}
	for id, want := range map[string]bool{root: true, mid: true, leaf: true, old: false} {
		if _, kept := memory.Runs[id]; kept != want {
			t.Errorf("run %s kept %v in its trace, want %v", id, kept, want)
		}
	}
	if _, named := memory.ToolNames[old]; named || !memory.Root {
		t.Errorf("the old tool's name was kept, or the trace forgot its root was seen: %+v", memory)
	}
}

// TestAnOlderMemoryIsForgotten. What version 1 remembered says nothing about
// whether a run's start was seen, so it is not read as version 2.
func TestAnOlderMemoryIsForgotten(t *testing.T) {
	path := filepath.Join(t.TempDir(), pendingFile)
	old := `{"version":1,"runs":{"` + inheritChild + `":{"trace":"` + inheritRoot + `","session":"ls-remembered"}}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if open := loadPending(path); len(open.Runs) != 0 || open.Version != pendingVersion {
		t.Errorf("version %d with %d runs: an older memory must be forgotten", open.Version, len(open.Runs))
	}
}
