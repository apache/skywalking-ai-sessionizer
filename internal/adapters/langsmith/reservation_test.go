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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func requestBody(n, size int) []byte {
	unit := []byte(fmt.Sprintf("{\"run\":%d}", n))
	return bytes.Repeat(unit, size/len(unit)+1)[:size]
}

// reservingProducer stands for a receiver on a file system with neither an
// exclusive rename nor hard links. There, the last resort of WriteExclusive
// (internal/storage/exclusive_fallback.go) makes the request's name as an
// empty file first, then renames the complete request over it. reserve and
// publish are those two steps, so a test can stop between them.
type reservingProducer struct {
	dir string
	seq uint64
}

func (p *reservingProducer) reserve() (string, error) {
	p.seq++
	name := fmt.Sprintf("runs-%s-%06d.request", storage.Stamp(time.Now()), p.seq)
	path := filepath.Join(p.dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	return path, file.Close()
}

func (p *reservingProducer) publish(path string, body []byte) error {
	header, err := json.Marshal(Received{V: 1, At: time.Now().UTC().Format(time.RFC3339Nano),
		ContentType: "application/json", Method: "POST", Path: "/runs", Bytes: len(body)})
	if err != nil {
		return err
	}
	temporary := filepath.Join(p.dir, ".tmp-"+filepath.Base(path))
	if err := os.WriteFile(temporary, append(append(header, '\n'), body...), 0o444); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// publishRaw is publish for a captured request with its own content type.
func (p *reservingProducer) publishRaw(path, contentType string, body []byte) error {
	header, err := json.Marshal(Received{V: 1, At: time.Now().UTC().Format(time.RFC3339Nano),
		ContentType: contentType, Method: "POST", Path: "/runs/multipart", Bytes: len(body)})
	if err != nil {
		return err
	}
	temporary := filepath.Join(p.dir, ".tmp-"+filepath.Base(path))
	if err := os.WriteFile(temporary, append(append(header, '\n'), body...), 0o444); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// produce reserves then publishes one request, the way the last resort does.
func (p *reservingProducer) produce(body []byte) error {
	path, err := p.reserve()
	if err != nil {
		return err
	}
	return p.publish(path, body)
}

func newProducer(t *testing.T, root string) *reservingProducer {
	t.Helper()
	dir := NewInbox(storage.NewZone(root)).Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &reservingProducer{dir: dir}
}

// Collection passes over a reservation and leaves it, consumes the complete
// requests beside it, and consumes the reserved one once it arrives whole.
func TestListPassesOverAnEmptyReservation(t *testing.T) {
	root := t.TempDir()
	producer := newProducer(t, root)
	collector := NewInbox(storage.NewZone(root))
	first, err := producer.reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.publish(first, requestBody(1, 1000)); err != nil {
		t.Fatal(err)
	}
	second, err := producer.reserve()
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := collector.List()
	if err != nil {
		t.Fatalf("collection stopped at the reservation: %v", err)
	}
	if len(waiting) != 1 || waiting[0].Path != first {
		t.Fatalf("waiting %+v", waiting)
	}
	if info, err := os.Stat(second); err != nil || info.Size() != 0 {
		t.Fatalf("the reservation was touched: %v, %v", info, err)
	}
	if err := producer.publish(second, requestBody(2, 1000)); err != nil {
		t.Fatal(err)
	}
	waiting, err = collector.List()
	if err != nil || len(waiting) != 2 {
		t.Fatalf("after the replacement: %+v, %v", waiting, err)
	}
	if err := collector.Read(&waiting[1]); err != nil || !bytes.Equal(waiting[1].Body, requestBody(2, 1000)) {
		t.Fatalf("the reserved request was not consumed whole: %v", err)
	}
}

// A producer that died inside the window leaves an empty reservation. It
// stays, harmless: a restarted receiver numbers past it, and its later
// requests are consumed. A non-empty request that cannot be read still fails.
func TestALeftReservationIsHarmless(t *testing.T) {
	root := t.TempDir()
	receiver := NewInbox(storage.NewZone(root))
	if _, err := receiver.Put("application/json", "POST", "/runs", requestBody(1, 100), time.Now()); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(receiver.Dir(), fmt.Sprintf("runs-%s-%06d.request", storage.Stamp(time.Now()), 2))
	if err := os.WriteFile(left, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := NewInbox(storage.NewZone(root))
	path, err := restarted.Put("application/json", "POST", "/runs", requestBody(3, 100), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := restarted.List()
	if err != nil || len(waiting) != 2 || waiting[1].Path != path || waiting[1].Seq != 3 {
		t.Fatalf("waiting %+v, %v", waiting, err)
	}
	for _, w := range waiting {
		if err := restarted.Done(w.Path); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(left); err != nil || info.Size() != 0 {
		t.Fatalf("the left reservation was touched: %v, %v", info, err)
	}
	malformed := filepath.Join(receiver.Dir(), fmt.Sprintf("runs-%s-%06d.request", storage.Stamp(time.Now()), 9))
	if err := os.WriteFile(malformed, []byte("not a request"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.List(); err == nil {
		t.Fatal("a malformed request was passed over")
	}
}

// drainAll consumes requests while produce runs in another goroutine, then
// once more after it ends, and requires every wanted body once and whole.
func drainAll(t *testing.T, collector *Inbox, want map[[sha256.Size]byte]bool, produce func() error) {
	t.Helper()
	produced := make(chan error, 1)
	go func() { produced <- produce() }()
	got := map[[sha256.Size]byte]int{}
	drain := func() {
		waiting, err := collector.List()
		if err != nil {
			t.Fatalf("collection: %v", err)
		}
		for i := range waiting {
			if err := collector.Read(&waiting[i]); err != nil {
				t.Fatal(err)
			}
			got[sha256.Sum256(waiting[i].Body)]++
			if err := collector.Done(waiting[i].Path); err != nil {
				t.Fatal(err)
			}
		}
	}
	for done := false; !done; {
		select {
		case err := <-produced:
			if err != nil {
				t.Fatal(err)
			}
			done = true
		default:
		}
		drain()
	}
	if len(got) != len(want) {
		t.Fatalf("consumed %d of %d bodies", len(got), len(want))
	}
	for sum, count := range got {
		if !want[sum] || count != 1 {
			t.Fatalf("a body consumed %d times, or not whole", count)
		}
	}
}

// A reserving producer and a collector overlap: every request is consumed
// whole and once, however its reservation and its replacement fall between
// the collector's passes.
func TestOverlappingReservationsAreConsumedWhole(t *testing.T) {
	root := t.TempDir()
	producer := newProducer(t, root)
	collector := NewInbox(storage.NewZone(root))
	const requests = 40
	want := map[[sha256.Size]byte]bool{}
	for n := 0; n < requests; n++ {
		want[sha256.Sum256(requestBody(n, 256<<10))] = true
	}
	drainAll(t, collector, want, func() error {
		for n := 0; n < requests; n++ {
			if err := producer.produce(requestBody(n, 256<<10)); err != nil {
				return err
			}
		}
		return nil
	})
}

// The same overlap on the native install path, through a real receiver's Inbox.
func TestOverlappingRequestsAreConsumedWhole(t *testing.T) {
	root := t.TempDir()
	receiver := NewInbox(storage.NewZone(root))
	collector := NewInbox(storage.NewZone(root))
	const requests = 40
	want := map[[sha256.Size]byte]bool{}
	for n := 0; n < requests; n++ {
		want[sha256.Sum256(requestBody(n, 256<<10))] = true
	}
	drainAll(t, collector, want, func() error {
		for n := 0; n < requests; n++ {
			if _, err := receiver.Put("application/json", "POST", "/runs", requestBody(n, 256<<10), time.Now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// capturedRequest is one request a real LangSmith client sent, from the
// captured corpus.
type capturedRequest struct {
	contentType string
	body        []byte
}

func corpusRequests(t *testing.T) []capturedRequest {
	t.Helper()
	var requests []capturedRequest
	captured(t, func(_, _ string, meta capturedMeta, body []byte) {
		requests = append(requests, capturedRequest{meta.Headers["Content-Type"], body})
	})
	return requests
}

// owningCollector is a real collector with the default ownership, as the
// receiver's own tests use, on a fixed clock.
func owningCollector(zone *storage.Zone) *Collector {
	at := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	return &Collector{Zone: zone, Ownership: DefaultOwnership(),
		Now: func() time.Time { at = at.Add(time.Second); return at }}
}

// collectUntilEmpty runs passes until the inbox is empty and adds up what they
// landed.
func collectUntilEmpty(t *testing.T, collector *Collector, total *Landed) {
	t.Helper()
	for {
		landed, err := collector.Collect()
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		total.Requests += landed.Requests
		total.Records += landed.Records
		total.Sessions = append(total.Sessions, landed.Sessions...)
		waiting, err := NewInbox(collector.Zone).List()
		if err != nil {
			t.Fatal(err)
		}
		if len(waiting) == 0 {
			return
		}
	}
}

func sessionSet(sessions []string) map[string]bool {
	set := map[string]bool{}
	for _, s := range sessions {
		set[s] = true
	}
	return set
}

// A real receiver accepts the captured requests over HTTP while a real
// collector runs passes beside it: every request lands, with the records and
// sessions a run that collects only after the last request lands too.
func TestReceiverAndCollectorOverlap(t *testing.T) {
	requests := corpusRequests(t)
	_, zone, base := started(t, "")
	collector := owningCollector(zone)
	sent := make(chan error, 1)
	go func() {
		for _, r := range requests {
			resp, err := http.Post(base+"/runs/multipart", r.contentType, bytes.NewReader(r.body))
			if err != nil {
				sent <- err
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				sent <- fmt.Errorf("status %d", resp.StatusCode)
				return
			}
		}
		sent <- nil
	}()
	var overlapped Landed
	for done := false; !done; {
		select {
		case err := <-sent:
			if err != nil {
				t.Fatal(err)
			}
			done = true
		default:
		}
		collectUntilEmpty(t, collector, &overlapped)
	}

	_, after, afterBase := started(t, "")
	for _, r := range requests {
		resp, err := http.Post(afterBase+"/runs/multipart", r.contentType, bytes.NewReader(r.body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	var reference Landed
	collectUntilEmpty(t, owningCollector(after), &reference)
	if overlapped.Requests != len(requests) || overlapped.Records != reference.Records ||
		!reflect.DeepEqual(sessionSet(overlapped.Sessions), sessionSet(reference.Sessions)) {
		t.Fatalf("overlapped %d requests, %d records, %d sessions; after the fact %d, %d, %d",
			overlapped.Requests, overlapped.Records, len(sessionSet(overlapped.Sessions)),
			reference.Requests, reference.Records, len(sessionSet(reference.Sessions)))
	}
}

// A real collection pass lands the complete requests and passes over a
// reservation, which it lands once the request is renamed over it.
func TestCollectionPassesOverAReservation(t *testing.T) {
	requests := corpusRequests(t)
	if len(requests) < 2 {
		t.Skip("the corpus holds fewer than two requests")
	}
	zone := storage.NewZone(t.TempDir())
	receiver := NewInbox(zone)
	if _, err := receiver.Put(requests[0].contentType, "POST", "/runs/multipart", requests[0].body, time.Now()); err != nil {
		t.Fatal(err)
	}
	producer := &reservingProducer{dir: receiver.Dir(), seq: 100}
	reservation, err := producer.reserve()
	if err != nil {
		t.Fatal(err)
	}
	collector := owningCollector(zone)
	landed, err := collector.Collect()
	if err != nil || landed.Requests != 1 {
		t.Fatalf("a pass beside a reservation: %+v, %v", landed, err)
	}
	if info, err := os.Stat(reservation); err != nil || info.Size() != 0 {
		t.Fatalf("the reservation was touched: %v, %v", info, err)
	}
	if err := producer.publishRaw(reservation, requests[1].contentType, requests[1].body); err != nil {
		t.Fatal(err)
	}
	landed, err = collector.Collect()
	if err != nil || landed.Requests != 1 {
		t.Fatalf("the replacement: %+v, %v", landed, err)
	}
}

// TestReservingProducerProcess is the producer that dies inside the window: it
// reserves a name and exits without renaming anything over it.
func TestReservingProducerProcess(t *testing.T) {
	dir := os.Getenv("ASZ_RESERVE_DIR")
	if dir == "" {
		return
	}
	producer := &reservingProducer{dir: dir, seq: 500}
	if _, err := producer.reserve(); err != nil {
		t.Fatal(err)
	}
	os.Exit(3)
}

// A producer process dies with its reservation in place. A restarted receiver
// takes later requests, collection lands every one of them, and the empty
// reservation stays as it was.
func TestAProducerDiesInsideTheWindow(t *testing.T) {
	requests := corpusRequests(t)
	zone := storage.NewZone(t.TempDir())
	inbox := NewInbox(zone)
	if err := os.MkdirAll(inbox.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestReservingProducerProcess$")
	child.Env = append(os.Environ(), "ASZ_RESERVE_DIR="+inbox.Dir())
	err = child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the producer did not die inside the window: %v", err)
	}
	entries, err := os.ReadDir(inbox.Dir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("after the producer died: %v, %v", entries, err)
	}
	left := filepath.Join(inbox.Dir(), entries[0].Name())

	r := &Receiver{Zone: zone, Listen: "127.0.0.1:0"}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	for _, req := range requests {
		resp, err := http.Post("http://"+r.Addr()+"/runs/multipart", req.contentType, bytes.NewReader(req.body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	var total Landed
	collectUntilEmpty(t, owningCollector(zone), &total)
	if total.Requests != len(requests) {
		t.Fatalf("landed %d of %d requests", total.Requests, len(requests))
	}
	if info, err := os.Stat(left); err != nil || info.Size() != 0 {
		t.Fatalf("the left reservation was touched: %v, %v", info, err)
	}
}
