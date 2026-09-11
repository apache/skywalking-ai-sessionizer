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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/remove"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// elsewhere names a receiver no check sends to. Nothing listens there, and
// nothing tries to reach it. Only the removal compares it with push.state.
const elsewhere = "grpc://another-receiver.invalid:4317"

// retention is the policy the retention case writes into its copy's marker.
const retention = 24 * time.Hour

// removal is one run of the removed_after_sent property over one format. It
// only reads the finished root. Every case works on a full copy of it, and
// every copy sends to the one receiver the run starts.
type removal struct {
	sc       *scenario.Scenario
	format   scenario.Format
	out      string
	session  string
	project  string
	maxRound int64
	opts     Options
	rcv      *otlptest.Receiver
	client   otlp.Client
	endpoint string
	copies   int
	problems []string
}

// removedAfterSent checks what a pipeline's removal does with a scenario
// session. Nothing goes before all of it is recorded as sent to the receiver
// the pipeline sends to. All of it goes once it is, by the policy in its
// marker, and nothing is landed, derived, parsed or sent again afterwards. A
// session with no marker, which every real Claude Code session is, stays.
func removedAfterSent(sc *scenario.Scenario, f scenario.Format, out, session, project string, maxRound int64, opts Options) ([]string, error) {
	rcv, err := otlptest.Start()
	if err != nil {
		return nil, err
	}
	defer rcv.Close()
	o := rcv.Options(otlp.ProtocolGRPC)
	client, err := otlp.NewClient(o)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	x := &removal{sc: sc, format: f, out: out, session: session, project: project, maxRound: maxRound, opts: opts,
		rcv: rcv, client: client, endpoint: otlp.EndpointOf(o.Protocol, o.Endpoint, o.TLS)}
	type step struct {
		name string
		fn   func(*removalCase) error
	}
	cases := []step{
		{"before any push", (*removalCase).beforeAnyPush},
		{"a refused push", (*removalCase).refused},
		{"a partial success", (*removalCase).rejected},
		{"another endpoint", (*removalCase).anotherEndpoint},
		{"no marker", (*removalCase).noMarker},
		{"immediately", (*removalCase).immediately},
		{"a retention of " + retention.String(), (*removalCase).retained},
	}
	if f != scenario.FormatClaudeCode {
		cases = []step{{"an sd build", (*removalCase).neverMarked}}
	}
	for _, c := range cases {
		if err := x.run(c.name, c.fn); err != nil {
			return nil, fmt.Errorf("removed_after_sent: %s: %w", c.name, err)
		}
	}
	return x.problems, nil
}

// run makes a fresh copy of the finished root and runs one case on it. The
// copy's push.state is deleted first: push_follows_the_wire leaves one
// behind, and a case must start with nothing sent. The receiver starts each
// case empty and taking everything.
func (x *removal) run(name string, fn func(*removalCase) error) error {
	x.copies++
	dir := fmt.Sprintf("%s-removal-%d", x.out, x.copies)
	defer os.RemoveAll(dir)
	if err := copyRoot(x.out, dir); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, otlp.StateFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	x.rcv.Fail(false)
	x.rcv.Reject(0)
	x.rcv.Reset()
	c := &removalCase{removal: x, name: name, dir: dir, source: filepath.Join(dir, "_source"), zone: storage.NewZone(dir)}
	c.rm = c.remover()
	return fn(c)
}

// removalCase is one case, on its own copy of the root.
type removalCase struct {
	*removal
	name   string
	dir    string
	source string
	zone   *storage.Zone
	rm     *remove.Remover
}

func (c *removalCase) bad(format string, a ...any) {
	c.problems = append(c.problems, "removed_after_sent: "+c.name+": "+fmt.Sprintf(format, a...))
}

func (c *removalCase) changesRoot() string { return filepath.Join(c.source, "plugins", "data") }
func (c *removalCase) markerPath() string  { return scenario.MarkerPath(c.source, c.session) }
func (c *removalCase) chainDir() string    { return filepath.Join(c.dir, "_conversations", c.session) }

func (c *removalCase) sourcePath(rel string) string {
	return filepath.Join(c.source, filepath.FromSlash(rel))
}

// rel is a path under the copy as push.state writes it.
func (c *removalCase) rel(p string) string {
	rel, err := filepath.Rel(c.dir, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// remover is the removal a pipeline over the copy runs, as the build's
// configuration sets the pipeline up: both adapters, the changes adapter
// reading where the build wrote the plugin's output, and metrics derived.
func (c *removalCase) remover() *remove.Remover {
	return &remove.Remover{Zone: c.zone, Source: c.source,
		Local:   claudecode.New(c.source, c.zone, 0),
		Changes: claudecodechanges.New(c.changesRoot(), c.zone, 0), ChangesRoot: c.changesRoot(),
		Derives: true, Now: checkNow}
}

// push sends what the copy has not sent yet, and records the check's
// receiver in push.state, as the pipeline's pusher does.
func (c *removalCase) push() (*otlp.Stats, error) {
	p := &otlp.Pusher{Zone: c.zone, Client: c.client, Endpoint: c.endpoint, Version: "check",
		ServiceName: "Scenario Check", InstanceID: "scenario-check", Layer: "AI_AGENT", Now: checkNow}
	return p.Pass()
}

// prepare is a pass of the pipeline up to the push: it lands with both
// adapters, derives, and parses every session directory.
func (c *removalCase) prepare() error {
	if _, err := collect(c.format, c.dir); err != nil {
		return err
	}
	if _, err := derive(c.dir); err != nil {
		return err
	}
	_, err := parseEvery(c.dir, c.maxRound)
	return err
}

// send is a pass of the pipeline up to the removal, to a receiver that
// takes everything.
func (c *removalCase) send() (*otlp.Stats, error) {
	if err := c.prepare(); err != nil {
		return nil, err
	}
	st, err := c.push()
	if err != nil {
		return nil, err
	}
	if len(st.Errors) != 0 {
		return nil, fmt.Errorf("push: %v", st.Errors)
	}
	return st, nil
}

// removeNow is the last act of a pass. An error the removal reports is a
// problem of the property, so the other cases still run.
func (c *removalCase) removeNow(endpoint string) *remove.Result {
	res := c.rm.Pass(remove.Input{Endpoint: endpoint, SendLogs: true, SendMetrics: true})
	for _, err := range res.Errors {
		c.bad("the removal reported %v", err)
	}
	return res
}

// beforeAnyPush. Nothing is sent, so nothing is proved sent. The marker
// stays as the build wrote it, since a removal begins by rewriting it.
func (c *removalCase) beforeAnyPush() error {
	if err := c.prepare(); err != nil {
		return err
	}
	res := c.removeNow(c.endpoint)
	if len(res.Removed) != 0 {
		c.bad("removed %v before anything was sent", res.Removed)
	}
	m, err := scenario.ReadMarker(c.markerPath())
	if err != nil {
		return err
	}
	if m.State != scenario.MarkerWritten {
		c.bad("the marker says %s after a pass that removed nothing, not %s", m.State, scenario.MarkerWritten)
	}
	return nil
}

// refused. A receiver that refuses a request holds none of it. Nothing is
// recorded as sent, so nothing is removed.
func (c *removalCase) refused() error {
	if err := c.prepare(); err != nil {
		return err
	}
	c.rcv.Fail(true)
	st, err := c.push()
	c.rcv.Fail(false)
	if err != nil {
		return err
	}
	if st.Files != 0 || len(st.Errors) == 0 {
		c.bad("a push the receiver refused marked %d file(s) sent and reported %d error(s)", st.Files, len(st.Errors))
	}
	if res := c.removeNow(c.endpoint); len(res.Removed) != 0 {
		c.bad("removed %v after the receiver refused every request", res.Removed)
	}
	return nil
}

// rejected. A receiver that took a request and rejected records of it holds
// less than was sent. The session stays, and the pass says why. That record
// has to survive every later save of push.state, or the session would go at
// the next pass: a second session, sent whole, makes one.
func (c *removalCase) rejected() error {
	c.rcv.Reject(1)
	st, err := c.send()
	c.rcv.Reject(0)
	if err != nil {
		return err
	}
	if st.Files == 0 {
		c.bad("a partial success marked no file sent; OTLP says not to send such a request again")
	}
	res := c.removeNow(c.endpoint)
	if len(res.Removed) != 0 || !says(res.Kept, c.session, "rejected records") {
		c.bad("after a partial success: removed %v, kept %v; the session must stay, and the pass must say the receiver rejected records", res.Removed, res.Kept)
	}

	other := *c.sc
	other.Session = anotherSession(c.session)
	b, err := scenario.Build(&other, scenario.FormatClaudeCode, c.dir, scenario.Options{At: c.opts.At, Scale: c.opts.Scale, Interval: c.opts.Interval})
	if err != nil {
		return err
	}
	if st, err = c.send(); err != nil {
		return err
	}
	if st.Files == 0 {
		c.bad("the second session sent nothing, so push.state was not saved again")
	}
	res = c.removeNow(c.endpoint)
	if len(res.Removed) != 1 || res.Removed[0] != b.Session {
		c.bad("removed %v; want only %s, the second session, which was sent whole", res.Removed, b.Session)
	}
	if !present(c.markerPath()) || !present(c.zone.SessionDir(c.session)) {
		c.bad("the session whose records were rejected was removed after a later save of push.state")
	}
	sent, err := otlp.LoadSent(c.dir)
	if err != nil {
		return err
	}
	landed, err := storage.LandedFiles(c.zone, c.session)
	if err != nil {
		return err
	}
	for _, lf := range landed {
		if rel := c.rel(lf.Path); !sent.Rejected(rel) {
			c.bad("push.state no longer records that the receiver rejected records of %s", rel)
		}
	}
	return nil
}

// anotherEndpoint. push.state records which receivers files went to, not
// which file went to which. Sent somewhere other than where this pipeline
// sends, nothing is proved sent to it, and every session stays.
func (c *removalCase) anotherEndpoint() error {
	if _, err := c.send(); err != nil {
		return err
	}
	res := c.removeNow(elsewhere)
	if len(res.Removed) != 0 || !says(res.Kept, "", "push.state records sending to "+c.endpoint) {
		c.bad("sent to %s and removing for %s: removed %v, kept %v", c.endpoint, elsewhere, res.Removed, res.Kept)
	}
	if !present(c.markerPath()) {
		c.bad("the marker was deleted")
	}
	return nil
}

// noMarker. A session no build marked, as every real Claude Code session
// is, stays whatever was sent. Nothing of it is touched, and nothing is said
// about it.
func (c *removalCase) noMarker() error {
	m, err := scenario.ReadMarker(c.markerPath())
	if err != nil {
		return err
	}
	if err := os.Remove(c.markerPath()); err != nil {
		return err
	}
	if _, err := c.send(); err != nil {
		return err
	}
	res := c.removeNow(c.endpoint)
	if len(res.Removed) != 0 || len(res.Kept) != 0 {
		c.bad("with no marker: removed %v, kept %v", res.Removed, res.Kept)
	}
	for _, f := range m.Files {
		if !present(c.sourcePath(f.Path)) {
			c.bad("the source file %s was deleted", f.Path)
		}
	}
	for _, p := range []string{c.zone.SessionDir(c.session), c.chainDir()} {
		if !present(p) {
			c.bad("%s was deleted", c.rel(p))
		}
	}
	return nil
}

// immediately. Once all of the session is sent, one pass removes all of it,
// and nothing that other sessions share. Afterwards nothing lands, derives,
// parses or sends it again, and a second removal finds nothing to do.
func (c *removalCase) immediately() error {
	m, err := scenario.ReadMarker(c.markerPath())
	if err != nil {
		return err
	}
	if _, err := c.send(); err != nil {
		return err
	}
	res := c.removeNow(c.endpoint)
	if len(res.Removed) != 1 || res.Removed[0] != c.session {
		c.bad("removed %v, kept %v, waiting %v; the session was sent whole and must go", res.Removed, res.Kept, res.Waiting)
		return nil
	}
	if err := c.goneWhole(m, "after the removal"); err != nil {
		return err
	}

	c.rcv.Reset()
	landed, err := collect(c.format, c.dir)
	if err != nil {
		return err
	}
	if landed != 0 {
		c.bad("afterwards the adapters landed %d source(s)", landed)
	}
	derived, err := derive(c.dir)
	if err != nil {
		return err
	}
	if derived != 0 {
		c.bad("afterwards the deriver wrote %d request(s)", derived)
	}
	rounds, err := parseEvery(c.dir, c.maxRound)
	if err != nil {
		return err
	}
	if rounds != 0 {
		c.bad("afterwards the parse wrote %d round(s)", rounds)
	}
	st, err := c.push()
	if err != nil {
		return err
	}
	if len(st.Errors) != 0 {
		return fmt.Errorf("push: %v", st.Errors)
	}
	if n := len(c.rcv.Requests()) + len(c.rcv.MetricsRequests()); n != 0 {
		c.bad("afterwards the push sent %d request(s)", n)
	}
	if again := c.removeNow(c.endpoint); len(again.Removed) != 0 {
		c.bad("a second removal removed %v", again.Removed)
	}
	return c.goneWhole(m, "afterwards")
}

// retained. A retained session stays until its last record is as old as
// the policy says. The age counts from the records, never from when anything
// ran, so the same data decides the same way.
func (c *removalCase) retained() error {
	m, err := scenario.ReadMarker(c.markerPath())
	if err != nil {
		return err
	}
	m.Policy, m.Retain = scenario.PolicyRetain, retention.String()
	if err := scenario.WriteMarker(c.markerPath(), m); err != nil {
		return err
	}
	last, err := lastSourceTime(c.source, m)
	if err != nil {
		c.bad("%v", err)
		return nil
	}
	if _, err := c.send(); err != nil {
		return err
	}
	now := last.Add(retention - time.Second)
	c.rm.Now = func() time.Time { return now }
	res := c.removeNow(c.endpoint)
	if len(res.Removed) != 0 || !says(res.Waiting, c.session, "kept until") {
		c.bad("a second before its last record, at %s, is %s old: removed %v, waiting %v; it must wait", last.Format(time.RFC3339Nano), retention, res.Removed, res.Waiting)
	}
	// The same remover, so what it remembers of the last record is used too.
	now = last.Add(retention)
	res = c.removeNow(c.endpoint)
	if len(res.Removed) != 1 || res.Removed[0] != c.session {
		c.bad("once its last record, at %s, is %s old: removed %v, waiting %v, kept %v; it must go", last.Format(time.RFC3339Nano), retention, res.Removed, res.Waiting, res.Kept)
		return nil
	}
	return c.goneWhole(m, "after the retention")
}

// neverMarked. An sd build writes no source, so it writes no marker and no
// sign of a scenario root, and nothing of it is removed.
func (c *removalCase) neverMarked() error {
	for _, p := range []string{filepath.Join(c.source, scenario.MarkerDir), filepath.Join(c.dir, storage.ScenarioDir)} {
		if present(p) {
			c.bad("an sd build left %s", c.rel(p))
		}
	}
	if _, err := c.send(); err != nil {
		return err
	}
	res := c.removeNow(c.endpoint)
	if len(res.Removed) != 0 || len(res.Kept) != 0 {
		c.bad("removed %v, kept %v", res.Removed, res.Kept)
	}
	for _, p := range []string{c.zone.SessionDir(c.session), c.chainDir()} {
		if !present(p) {
			c.bad("%s was deleted", c.rel(p))
		}
	}
	return nil
}

// goneWhole checks that nothing of the session is left, and that nothing
// other sessions share went with it.
func (c *removalCase) goneWhole(m *scenario.Marker, when string) error {
	bad := func(format string, a ...any) { c.bad(when+": "+format, a...) }
	s := c.session
	if present(c.markerPath()) {
		bad("the marker is still there")
	}
	shared := map[string]bool{}
	for _, f := range m.Files {
		if present(c.sourcePath(f.Path)) {
			bad("the source file %s is still there", f.Path)
		}
		above, own := sessionDirOf(f.Path, s)
		if own != "" && present(c.sourcePath(own)) {
			bad("the directory %s, named for the session, is still there", own)
		}
		shared[above] = true
	}
	for _, dir := range keys(shared) {
		if !present(c.sourcePath(dir)) {
			bad("%s, which other sessions share, was removed", dir)
		}
	}
	project := filepath.Join(c.source, filepath.FromSlash(c.project))
	for _, p := range []string{
		filepath.Join(project, "not-a-uuid.jsonl"),
		filepath.Join(project, "memory", "notes.md"),
		filepath.Join(c.source, scenario.MarkerDir),
		filepath.Join(c.dir, storage.ScenarioDir),
	} {
		if !present(p) {
			bad("%s, which every session shares, was removed", c.rel(p))
		}
	}
	for _, p := range []string{c.zone.SessionDir(s), c.chainDir()} {
		if present(p) {
			bad("%s is still there", c.rel(p))
		}
	}
	spool, err := os.ReadDir(filepath.Join(c.dir, storage.SpoolDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, it := range spool {
		if owner, ok := metrics.SpoolOwner(it.Name()); ok && owner == s {
			bad("the spool file %s is still there", it.Name())
		}
	}
	for _, state := range []string{filepath.Join(c.dir, otlp.StateFile), filepath.Join(c.dir, storage.SpoolDir, metrics.StateFile)} {
		data, err := os.ReadFile(state)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if bytes.Contains(data, []byte(s)) {
			bad("%s still names the session", c.rel(state))
		}
	}
	left, err := os.ReadDir(filepath.Join(c.dir, storage.RemovedDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(left) != 0 {
		bad("%s still holds %d entries", storage.RemovedDir, len(left))
	}
	ids, err := view.New(c.zone, nil).List()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if id == s {
			bad("the page still lists the conversation")
		}
	}
	return nil
}

// sessionDirOf splits a marker path at its first segment equal to the
// session. It returns the directory above that segment, which other sessions
// share, and the directory the segment names, which is the session's own. A
// path with no such segment, such as the main transcript, has no directory
// of its own, and its parent is shared.
func sessionDirOf(p, session string) (shared, own string) {
	parts := strings.Split(p, "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == session {
			return strings.Join(parts[:i], "/"), strings.Join(parts[:i+1], "/")
		}
	}
	return path.Dir(p), ""
}

// lastSourceTime is the latest time the session's source lines carry, read
// as each producer writes it: timestamp in Claude Code's own files, and time
// in the plugin's. It reads no landed file, so it checks the removal's
// reading of the landed records rather than repeat it.
func lastSourceTime(source string, m *scenario.Marker) (time.Time, error) {
	var last time.Time
	for _, f := range m.Files {
		key := "timestamp"
		if strings.HasPrefix(f.Path, "plugins/data/") {
			key = "time"
		}
		data, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(f.Path)))
		if err != nil {
			return last, err
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			var fields map[string]json.RawMessage
			if json.Unmarshal(line, &fields) != nil {
				continue
			}
			var s string
			if json.Unmarshal(fields[key], &s) != nil {
				continue
			}
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(last) {
				last = t
			}
		}
	}
	if last.IsZero() {
		return last, errors.New("no line of the session's source files carries a time, so no retention can be counted")
	}
	return last, nil
}

// parseEvery parses every session directory of a root, as a pass of the
// pipeline does, and returns how many rounds it wrote.
func parseEvery(root string, maxRound int64) (int, error) {
	items, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	z := storage.NewZone(root)
	written := 0
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), "_") {
			continue
		}
		for {
			r, err := parse.Session(z, parse.Options{Conversation: it.Name(), Session: it.Name(), MaxRoundBytes: maxRound})
			if err != nil {
				return written, fmt.Errorf("parse %s: %w", it.Name(), err)
			}
			if r.Changed() {
				written++
			}
			if !r.Changed() || !r.More {
				break
			}
		}
	}
	return written, nil
}

// copyRoot copies a root, modes kept. Nothing in a root records where the
// root is, so the copy is a root in its own right. The configuration is left
// out, because a build into the copy refuses one that names another
// directory.
func copyRoot(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		if rel == "asz.yaml" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file, and a root holds nothing else", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// anotherSession is a session id that is not the scenario's own.
func anotherSession(session string) string {
	id := "aaaaaaaa-bbbb-4ccc-8ddd-000000000001"
	if strings.EqualFold(id, session) {
		id = "aaaaaaaa-bbbb-4ccc-8ddd-000000000002"
	}
	return id
}

// says reports whether a list of kept or waiting sessions holds one with a
// reason that contains the words given. An empty session is every session.
func says(list []remove.Kept, session, reason string) bool {
	for _, k := range list {
		if k.Session == session && strings.Contains(k.Reason, reason) {
			return true
		}
	}
	return false
}

func present(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
