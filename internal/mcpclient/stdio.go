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

package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"time"
)

// stdioExchange speaks to a local server over its standard input and
// output, one JSON-RPC message per line.
type stdioExchange struct {
	s     Server
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stdout is the read end of the server's output, asz's own: Wait never
	// closes it, so nothing the server wrote before it ended is lost.
	stdout *os.File
	lines  chan line
	exited chan struct{}
	// done ends the reader once the exchange closed, so it never waits on
	// a line no one reads.
	done chan struct{}
	// abandoned are requests asz stopped waiting for, the probe left
	// unanswered: an answer that comes late is passed over.
	abandoned map[int]bool
	era       string
	// version is the one a legacy server's handshake agreed; a modern
	// request names Modern itself.
	version string
	next    int
}

// line is one message the server wrote, or why no more will come.
type line struct {
	b   []byte
	err error
}

// openStdio starts the server in a process group of its own, with asz's
// environment and the server's Env added, its standard error taken by
// s.Stderr as it comes.
func openStdio(_ context.Context, s Server) (*stdioExchange, error) {
	cmd := exec.Command(s.Command[0], s.Command[1:]...)
	cmd.Env = append(os.Environ(), s.Env...)
	if s.Stderr != nil {
		cmd.Stderr = s.Stderr
	}
	// A child of the server that keeps its pipes open must not hold asz:
	// the copying of its output ends this long after the server does.
	cmd.WaitDelay = 2 * time.Second
	ownGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, failure(ClassTransport, "the server's input could not be made")
	}
	// Wait closes a pipe exec makes once the server exits, whether or not
	// it was read to its end, so the output pipe is made here.
	stdout, w, err := os.Pipe()
	if err != nil {
		return nil, failure(ClassTransport, "the server's output could not be made")
	}
	cmd.Stdout = w
	err = cmd.Start()
	w.Close()
	if err != nil {
		stdout.Close()
		return nil, failure(ClassTransport, "the server %s did not start", baseName(s.Command[0]))
	}
	x := &stdioExchange{s: s, cmd: cmd, stdin: stdin, stdout: stdout, lines: make(chan line, 16), exited: make(chan struct{}),
		done: make(chan struct{}), abandoned: map[int]bool{}}
	go x.read(stdout)
	go func() { _ = cmd.Wait(); close(x.exited) }()
	return x, nil
}

// read reads the server's messages, one per line, each within MaxMessage:
// never bufio.Scanner, which fails quietly past its own bound.
func (x *stdioExchange) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		var b []byte
		for {
			frag, err := br.ReadSlice('\n')
			b = append(b, frag...)
			if len(b) > x.s.MaxMessage {
				x.deliver(line{err: failure(ClassTooLarge, "a message past %d bytes", x.s.MaxMessage)})
				return
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				x.deliver(line{err: failure(ClassTransport, "the server's output ended")})
				return
			}
			break
		}
		if !x.deliver(line{b: b}) {
			return
		}
	}
}

// deliver hands a line to the exchange, unless it closed.
func (x *stdioExchange) deliver(l line) bool {
	select {
	case x.lines <- l:
		return true
	case <-x.done:
		return false
	}
}

// send writes one message. A server that stops reading its input blocks
// the write, so a write is bounded by ctx: past it the server is ended,
// which ends the write.
func (x *stdioExchange) send(ctx context.Context, b []byte) error {
	written := make(chan error, 1)
	go func() {
		_, err := x.stdin.Write(append(b, '\n'))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			return failure(ClassTransport, "the server's input closed")
		}
		return nil
	case <-ctx.Done():
		killGroup(x.cmd)
		select {
		case <-written:
		case <-time.After(time.Second):
		}
		return failure(ClassTimeout, "the exchange did not end in time")
	}
}

// ask sends a request and waits for its answer. A server's notification is
// passed over. A legacy server's ping is answered, and any other request it
// sends is refused; a modern server sends none, so one is a broken
// protocol. When ctx ends first, an unanswered request other than the
// handshake is cancelled.
func (x *stdioExchange) ask(ctx context.Context, method string, params map[string]any, modern bool, wait time.Duration) (*message, error) {
	x.next++
	id := x.next
	if err := x.send(ctx, x.s.request(id, method, params, modern)); err != nil {
		return nil, err
	}
	var timeout <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}
	for {
		select {
		case <-ctx.Done():
			if method != "initialize" {
				x.cancel(id)
			}
			return nil, failure(ClassTimeout, "the exchange did not end in time")
		case <-timeout:
			x.abandoned[id] = true
			return nil, errProbeTimeout
		case l := <-x.lines:
			if l.err != nil {
				return nil, l.err
			}
			m, err := readMessage(l.b)
			if err != nil {
				return nil, err
			}
			switch {
			case m.isNotification():
			case m.isRequest() && x.era == EraModern:
				return nil, failure(ClassProtocol, "a modern server sent a request")
			case m.isRequest() && m.Method == "ping":
				if err := x.send(ctx, reply(m.ID, map[string]any{}, nil)); err != nil {
					return nil, err
				}
			case m.isRequest():
				if err := x.send(ctx, reply(m.ID, nil, &rpcError{Code: codeMethodNotFound, Message: "asz offers no " + m.Method})); err != nil {
					return nil, err
				}
			case m.answers(id):
				return m, nil
			case m.Method == "" && x.late(m):
			default:
				return nil, failure(ClassProtocol, "a response to no request asz sent")
			}
		}
	}
}

// late says whether a response answers a request asz stopped waiting for.
func (x *stdioExchange) late(m *message) bool {
	for id := range x.abandoned {
		if m.ID != nil && m.answers(id) {
			return true
		}
	}
	return false
}

// errProbeTimeout is the modern probe left unanswered.
var errProbeTimeout = errors.New("no answer to the probe")

// cancel tells the server a request is no longer wanted, and gives it a
// second to stop, the write included.
func (x *stdioExchange) cancel(id int) {
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if x.send(ctx, notification("notifications/cancelled", map[string]any{"requestId": id, "reason": "the fetch ran out of time"})) != nil {
		return
	}
	select {
	case <-x.exited:
	case <-ctx.Done():
	}
}

// detect finds the era: the modern probe, server/discover, answered with a
// result or an error only a modern server sends shows a modern server; any
// other answer, or none within the probe's time, a legacy one, which is then
// given its handshake.
func (x *stdioExchange) detect(ctx context.Context) error {
	if x.era != "" {
		return nil
	}
	m, err := x.ask(ctx, "server/discover", nil, true, x.s.Probe)
	switch {
	case errors.Is(err, errProbeTimeout):
	case err != nil:
		return err
	case m.Error == nil:
		var r struct {
			SupportedVersions []string `json:"supportedVersions"`
		}
		_ = json.Unmarshal(m.Result, &r)
		if r.SupportedVersions != nil && !slices.Contains(r.SupportedVersions, Modern) {
			return failure(ClassProtocol, "the server speaks %v and not %s", r.SupportedVersions, Modern)
		}
		x.era = EraModern
		return nil
	case modernError(m.Error):
		if m.Error.Code == codeUnsupportedVersion && !supportsModern(m.Error) {
			return failure(ClassProtocol, "the server speaks only modern versions asz does not")
		}
		x.era = EraModern
		return nil
	}
	x.era = EraLegacy
	m, err = x.ask(ctx, "initialize", x.s.initializeParams(), false, 0)
	if err != nil {
		return err
	}
	if m.Error != nil {
		return serverError(m.Error)
	}
	if x.version, err = negotiated(m.Result); err != nil {
		return err
	}
	return x.send(ctx, notification("notifications/initialized", nil))
}

func (x *stdioExchange) call(ctx context.Context, tool string, args map[string]any) (*Called, error) {
	if err := x.detect(ctx); err != nil {
		return nil, err
	}
	m, err := x.ask(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, x.era == EraModern, 0)
	if err != nil {
		return nil, err
	}
	if m.Error != nil {
		return nil, serverError(m.Error)
	}
	if err := complete(m.Result); err != nil {
		return nil, err
	}
	return &Called{Era: x.era, Version: x.versionSpoken(), Result: m.Result}, nil
}

// list lists the tools. Over stdio a client may pass over x-mcp-header
// annotations, which concern HTTP headers alone, so none is left out.
func (x *stdioExchange) list(ctx context.Context, pages int) (*Listed, error) {
	if err := x.detect(ctx); err != nil {
		return nil, err
	}
	out := &Listed{Era: x.era, Version: x.versionSpoken(), Complete: true}
	var cursor *string
	for page := 0; ; page++ {
		if page == pages {
			out.Complete = false
			return out, nil
		}
		m, err := x.ask(ctx, "tools/list", pageParams(cursor), x.era == EraModern, 0)
		if err != nil {
			return nil, err
		}
		if m.Error != nil {
			return nil, serverError(m.Error)
		}
		tools, next, err := listPage(m.Result)
		if err != nil {
			return nil, err
		}
		for _, raw := range tools {
			var t Tool
			if json.Unmarshal(raw, &t) == nil && t.Name != "" {
				out.Tools = append(out.Tools, t)
			}
		}
		if next == nil {
			return out, nil
		}
		cursor = next
	}
}

// secrets are none: a local server has no session.
func (x *stdioExchange) secrets() []string { return nil }

func (x *stdioExchange) versionSpoken() string {
	if x.era == EraModern {
		return Modern
	}
	return x.version
}

// close ends the server: its input closed, two seconds to end, then its
// process group ended, and the process reaped. Its output is closed last.
func (x *stdioExchange) close() {
	close(x.done)
	_ = x.stdin.Close()
	select {
	case <-x.exited:
	case <-time.After(2 * time.Second):
		killGroup(x.cmd)
		<-x.exited
	}
	_ = x.stdout.Close()
}

// baseName is a program's name without its directory, for a failure that
// never prints where it was found.
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}
