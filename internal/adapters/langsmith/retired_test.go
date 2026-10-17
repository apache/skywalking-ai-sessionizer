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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// retireSession marks a conversation retired and takes its directory away,
// as a finished retirement leaves it.
func retireSession(t *testing.T, z *storage.Zone, session string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(z.Root(), storage.RetiredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(z.RetiredPath(session), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteTree(z.SessionDir(session), nil); err != nil {
		t.Fatal(err)
	}
}

// TestARetiredConversationsRunsAreNotCollected. A conversation is retired
// after its function and its call landed. Later arrivals of it are not
// collected, and none waits: a run that names it, inside a run that never
// came; a call inside its function that names no thread; an update of its
// call. A run of another conversation in the same request lands in the same
// pass.
func TestARetiredConversationsRunsAreNotCollected(t *testing.T) {
	_, zone, base := started(t, "")
	c := collectorOver(zone)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-retired")+`,`+start+`]}`)
	passes(t, c, 1)
	retired := threadOf("t-retired")
	if !mentions(t, zone, retired, `"FakeListChatModel"`) {
		t.Fatal("the call did not land in its function's conversation")
	}
	retireSession(t, zone, retired)

	const call = "16161616-1616-7616-8616-161616161612"
	callOrder := inheritOrder + ".20260920T100002000000Z" + call
	another := strings.ReplaceAll(strings.ReplaceAll(inheritCall(``), inheritChild, call),
		inheritOrder+".20260920T100001000000Z"+call, callOrder)
	// It ran inside a run that never came, so as a live run it would wait.
	const missing = "16161616-1616-7616-8616-161616161613"
	named := `{"id":"` + lostStep + `","trace_id":"` + missing + `","parent_run_id":"` + missing + `",` +
		`"dotted_order":"20260920T100000000000Z` + missing + `.20260920T100001000000Z` + lostStep + `",` +
		`"run_type":"chain","name":"named","start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:02Z",` +
		`"session_name":"asz","extra":{"metadata":{"thread_id":"t-retired"},"runtime":{"library":"langsmith"}},` +
		`"outputs":{"answer":"named again"}}`
	live := rootRun(lostBelow, "asz", `"thread_id":"t-live"`, "a live one")
	postBatch(t, base, `{"post":[`+named+`,`+another+`,`+live+`],"patch":[`+
		sparse(inheritChild, inheritRoot, lostCallOrder, "the call's end")+`]}`)
	landed, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Waiting != 0 || landed.Dropped != 0 || len(landed.Retired) != 1 || landed.Retired[0] != retired {
		t.Fatalf("%+v: the retired runs must be left out at once, and only them", landed)
	}
	if !mentions(t, zone, threadOf("t-live"), "a live one") {
		t.Error("the run of another conversation did not land")
	}
	if _, err := os.Stat(zone.SessionDir(retired)); !os.IsNotExist(err) {
		t.Fatalf("the retired conversation's directory was made again: %v", err)
	}
	open := loadPending(filepath.Join(zone.Root(), InboxDir, pendingFile))
	for _, id := range []string{lostStep, call, inheritChild} {
		if open.Runs[id].Session != retired {
			t.Errorf("run %s is remembered in %q, want the retired conversation", id, open.Runs[id].Session)
		}
	}
}

// TestAnUpdateUnderARetiredRunWaitsForItsStart. A call's update, naming no
// conversation, comes before its start, inside a function whose
// conversation is retired. It is not taken for the function's while it
// waits: its start names a thread of its own, and both land there.
func TestAnUpdateUnderARetiredRunWaitsForItsStart(t *testing.T) {
	_, zone, base := started(t, "")
	c := collectorOver(zone)
	postBatch(t, base, `{"post":[`+inheritFunction("t-retired")+`]}`)
	passes(t, c, 1)
	retireSession(t, zone, threadOf("t-retired"))
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "its own end")+`]}`)
	if landed, err := c.Collect(); err != nil || landed.Waiting != 1 || len(landed.Retired) != 0 {
		t.Fatalf("%v, %+v: the update must wait for its start, not be retired", err, landed)
	}
	start := strings.Replace(inheritCall(`"thread_id":"t-own"`), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+start+`]}`)
	passes(t, c, 1)
	if !mentions(t, zone, threadOf("t-own"), "its own end") || !mentions(t, zone, threadOf("t-own"), `"FakeListChatModel"`) {
		t.Error("the call and its update are not in the call's own conversation")
	}
}

// TestARetiredUnassignedConversationStaysRetired. A trace with no thread
// anywhere landed in its own conversation, unassigned, which is then
// retired. A later update of its run that carries a thread stays in it, and
// is not collected.
func TestARetiredUnassignedConversationStaysRetired(t *testing.T) {
	_, zone, base := started(t, "")
	c := collectorOver(zone)
	postBatch(t, base, `{"post":[`+rootRun(lostStep, "asz", ``, "no thread anywhere")+`]}`)
	passes(t, c, 1)
	retireSession(t, zone, UnassignedID(lostStep))
	postBatch(t, base, `{"patch":[`+rootRun(lostStep, "asz", `"thread_id":"t-later"`, "a thread too late")+`]}`)
	landed, err := c.Collect()
	if err != nil || len(landed.Retired) != 1 || landed.Retired[0] != UnassignedID(lostStep) {
		t.Fatalf("%v, %+v: the update must stay in its retired conversation", err, landed)
	}
	if landedAnywhere(t, zone, "a thread too late") {
		t.Error("the update of a retired conversation's run was collected")
	}
}

// TestARetiredConversationAcrossRestartsAndWaits. A conversation is retired
// after its function and its call landed.
//   - After a restart, an update of its call is still retired: what is
//     remembered of the call names it.
//   - A request that holds an update of the retired call and an update whose
//     start never came waits as long as it may for the second. Then the first
//     is left out as retired and the second dropped, and nothing lands.
//   - Once the function is forgotten, a call inside it that names no thread
//     cannot be told and is dropped: it lands neither in the retired
//     conversation nor anywhere else.
func TestARetiredConversationAcrossRestartsAndWaits(t *testing.T) {
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-retired")+`,`+start+`]}`)
	passes(t, c, 1)
	retired := threadOf("t-retired")
	retireSession(t, zone, retired)

	restarted := &Collector{Zone: zone, Now: clock.now}
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "after a restart")+`]}`)
	if landed, err := restarted.Collect(); err != nil || len(landed.Retired) != 1 || landed.Retired[0] != retired {
		t.Fatalf("%v, %+v: the call's update is retired after a restart", err, landed)
	}

	order := "20260920T100000000000Z" + lostStep
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "beside one that waits")+`,`+
		sparse(lostStep, lostStep, order, "never started")+`]}`)
	dropped, retiredSeen := 0, false
	for pass := 0; pass <= deferLimit; pass++ {
		landed, err := restarted.Collect()
		if err != nil {
			t.Fatal(err)
		}
		dropped += landed.Dropped
		retiredSeen = retiredSeen || len(landed.Retired) == 1
	}
	if dropped != 1 || !retiredSeen || !inboxEmpty(t, zone) {
		t.Fatalf("%d dropped, retired seen %v: once the wait ends, the retired update is left out and the other dropped", dropped, retiredSeen)
	}

	clock.at = clock.at.Add(25 * time.Hour)
	postBatch(t, base, `{"post":[`+otherRun("t-other")+`]}`)
	const call = "16161616-1616-7616-8616-161616161614"
	late := strings.ReplaceAll(inheritCall(``), inheritChild, call)
	postBatch(t, base, `{"post":[`+late+`]}`)
	if dropped := passes(t, restarted, deferLimit+1); dropped != 1 {
		t.Fatalf("%d dropped: a call under a forgotten retired function cannot be told", dropped)
	}
	for _, text := range []string{"after a restart", "beside one that waits", "never started", call} {
		if landedAnywhere(t, zone, text) {
			t.Errorf("%q was collected", text)
		}
	}
}

// TestARetiredCallOutlivesItsFunction. A conversation is retired after its
// function and its call landed. The call is updated within the day, so it
// is still remembered when its function is forgotten. Its next update is
// still retired, by what is remembered of the call itself: it does not wait
// for the function, is not dropped, and lands nowhere.
func TestARetiredCallOutlivesItsFunction(t *testing.T) {
	_, zone, base := started(t, "")
	c, clock := clockedOver(zone)
	start := strings.Replace(inheritCall(``), `"end_time":"2026-09-20T10:00:02Z",`, ``, 1)
	postBatch(t, base, `{"post":[`+inheritFunction("t-retired")+`,`+start+`]}`)
	passes(t, c, 1)
	retired := threadOf("t-retired")
	retireSession(t, zone, retired)

	clock.at = clock.at.Add(12 * time.Hour)
	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "within the day")+`]}`)
	if landed, err := c.Collect(); err != nil || len(landed.Retired) != 1 || landed.Retired[0] != retired {
		t.Fatalf("%v, %+v: the call's update is retired", err, landed)
	}
	clock.at = clock.at.Add(13 * time.Hour)
	postBatch(t, base, `{"post":[`+otherRun("t-other")+`]}`)
	passes(t, c, 1)
	open := loadPending(filepath.Join(zone.Root(), InboxDir, pendingFile))
	if _, ok := open.Runs[inheritRoot]; ok {
		t.Fatal("the function is still remembered; the test needs it forgotten")
	}
	if open.Runs[inheritChild].Session != retired {
		t.Fatalf("the call is remembered in %q, want the retired conversation", open.Runs[inheritChild].Session)
	}

	postBatch(t, base, `{"patch":[`+sparse(inheritChild, inheritRoot, lostCallOrder, "after its function")+`]}`)
	landed, err := c.Collect()
	if err != nil || landed.Waiting != 0 || landed.Dropped != 0 || len(landed.Retired) != 1 || landed.Retired[0] != retired {
		t.Fatalf("%v, %+v: the call's update is still retired, at once", err, landed)
	}
	for _, text := range []string{"within the day", "after its function"} {
		if landedAnywhere(t, zone, text) {
			t.Errorf("%q was collected", text)
		}
	}
}

// TestAMarkerThatCannotBeReadKeepsTheRequest. While the markers cannot be
// read, no run's conversation can be told retired or not, so the request is
// kept, not landed and not dropped. Once they read, it lands.
func TestAMarkerThatCannotBeReadKeepsTheRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file where a directory should be reads as not there on Windows")
	}
	_, zone, base := started(t, "")
	blocked := filepath.Join(zone.Root(), storage.RetiredDir)
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	postBatch(t, base, `{"post":[`+rootRun(lostStep, "asz", `"thread_id":"t-live"`, "kept for later")+`]}`)
	c := collectorOver(zone)
	if _, err := c.Collect(); err == nil {
		t.Fatal("a pass that cannot read the markers succeeded")
	}
	if inboxEmpty(t, zone) || landedAnywhere(t, zone, "kept for later") {
		t.Fatal("the request was not kept for a later pass")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	passes(t, c, 1)
	if !mentions(t, zone, threadOf("t-live"), "kept for later") {
		t.Error("the request did not land once the markers read")
	}
}
