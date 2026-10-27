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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The test binary is also the local server the tests start: with
// MCPCLIENT_FAKE set it serves as that mode says, logging every line it
// reads to MCPCLIENT_FAKE_LOG, and never runs the tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("MCPCLIENT_FAKE"); mode != "" {
		fakeServer(mode, os.Getenv("MCPCLIENT_FAKE_LOG"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeServer answers one message per line on standard input. Its modes:
// modern, a server of 2026-07-28; other, one that supports only a modern
// version asz does not; legacy, one that refuses the probe as an unknown
// method; silent, a legacy one that never answers the probe; late, one that
// answers the probe only after the handshake; noid, a modern one that
// refuses the probe's version with an error that carries no id; and flood,
// one that writes 30 short lines as it starts and ends: 17 at once, which
// fill what asz takes before anyone reads them, and 13 after a pause, which
// stay in the pipe. The 13 come to 1.3 KiB: a pipe on Windows holds far
// less than one on Unix, and 40 lines of 512 bytes never let the server end
// there. After "+", any
// of these flags: requests, sending a ping and a sampling request before a
// call's answer; slow, never answering a call; slowinit, never answering
// the handshake; big, answering a call with a line past 1 KiB; exit, ending
// before a call's answer; child, starting a child that keeps the pipes
// open; stubborn, staying after its input closes; deaf, reading nothing
// after the probe; paged, listing tools over two pages, the second behind
// an empty cursor.
func fakeServer(mode, logPath string) {
	era, flags, _ := strings.Cut(mode, "+")
	has := func(f string) bool { return strings.Contains("+"+flags+"+", "+"+f+"+") }
	var log *os.File
	if logPath != "" {
		log, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	out := bufio.NewWriter(os.Stdout)
	write := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	answer := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	refuse := func(id json.RawMessage, code int) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": "no"}})
	}
	if has("child") && runtime.GOOS != "windows" {
		c := exec.Command("sleep", "30")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		_ = c.Start()
	}
	if era == "flood" {
		note := map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"data": strings.Repeat("n", 32)}}
		for i := range 30 {
			if i == 17 {
				time.Sleep(300 * time.Millisecond)
			}
			write(note)
		}
		return
	}
	var probe json.RawMessage
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			if has("stubborn") {
				time.Sleep(30 * time.Second)
			}
			return
		}
		if log != nil {
			log.Write(line)
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		_ = json.Unmarshal(line, &m)
		switch m.Method {
		case "server/discover":
			switch era {
			case "modern":
				answer(m.ID, map[string]any{"supportedVersions": []string{Modern}})
				if has("deaf") {
					time.Sleep(30 * time.Second)
					return
				}
			case "other":
				answer(m.ID, map[string]any{"supportedVersions": []string{"2099-01-01"}})
			case "legacy":
				refuse(m.ID, codeMethodNotFound)
			case "late":
				probe = m.ID
			case "noid":
				// An unsupported version can come with no id: it answers the
				// request in flight.
				write(map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": codeUnsupportedVersion, "message": "no",
					"data": map[string]any{"supported": []string{"2099-01-01"}}}})
			}
		case "initialize":
			if has("slowinit") {
				continue
			}
			if probe != nil {
				refuse(probe, codeMethodNotFound)
			}
			answer(m.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
		case "tools/list":
			cursor, more := m.Params["cursor"]
			switch {
			case has("paged") && !more:
				answer(m.ID, map[string]any{"tools": []any{map[string]any{"name": "get", "inputSchema": map[string]any{}}}, "nextCursor": ""})
			case has("paged") && cursor == "":
				answer(m.ID, map[string]any{"tools": []any{map[string]any{"name": "two", "inputSchema": map[string]any{}}}})
			default:
				answer(m.ID, map[string]any{"tools": []any{map[string]any{"name": "get", "inputSchema": map[string]any{}}}})
			}
		case "tools/call":
			switch {
			case has("slow"):
				continue
			case has("exit"):
				return
			case has("big"):
				answer(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 2048)}}})
				continue
			case has("requests"):
				write(map[string]any{"jsonrpc": "2.0", "id": "r1", "method": "ping"})
				write(map[string]any{"jsonrpc": "2.0", "id": "r2", "method": "sampling/createMessage", "params": map[string]any{}})
				// Two answers, read before the call's own: a server waits for
				// what it asked.
				for range 2 {
					reply, err := in.ReadBytes('\n')
					if err != nil {
						return
					}
					if log != nil {
						log.Write(reply)
					}
				}
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{}})
			answer(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "the issue"}}})
		}
	}
}

// local is a server this test binary acts in mode, its log in a temporary
// directory.
func local(t *testing.T, mode string) (Server, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "fake.log")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Server{Command: []string{exe}, Env: []string{"MCPCLIENT_FAKE=" + mode, "MCPCLIENT_FAKE_LOG=" + log}, Probe: 500 * time.Millisecond}, log
}

// took lists the methods a fake server read, and the ids of the answers it
// was sent, in order.
func took(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch {
		case m.Method != "":
			out = append(out, m.Method)
		case m.Error != nil:
			out = append(out, fmt.Sprintf("refused %s %d", m.ID, m.Error.Code))
		default:
			out = append(out, "answered "+string(m.ID))
		}
	}
	return out
}

func callLocal(t *testing.T, s Server, timeout time.Duration) (*Called, error) {
	t.Helper()
	ctx, stop := context.WithTimeout(context.Background(), timeout)
	defer stop()
	return Call(ctx, s, "get", map[string]any{"key": "OPS-1"})
}

// A local server is found modern by its answer to the probe, and called
// with the modern metadata and no handshake; one that refuses the probe as
// an unknown method, or never answers it, is given the legacy handshake.
// A probe answered late is passed over.
func TestALocalServersEraIsFoundByTheProbe(t *testing.T) {
	for _, c := range []struct {
		mode, era, version, took string
	}{
		{"modern", EraModern, Modern, "server/discover tools/call"},
		{"legacy", EraLegacy, "2025-06-18", "server/discover initialize notifications/initialized tools/call"},
		{"silent", EraLegacy, "2025-06-18", "server/discover initialize notifications/initialized tools/call"},
		{"late", EraLegacy, "2025-06-18", "server/discover initialize notifications/initialized tools/call"},
	} {
		s, log := local(t, c.mode)
		got, err := callLocal(t, s, 10*time.Second)
		if err != nil || got.Era != c.era || got.Version != c.version || !strings.Contains(string(got.Result), "the issue") {
			t.Fatalf("%s: %+v %v", c.mode, got, err)
		}
		if seen := strings.Join(took(t, log), " "); seen != c.took {
			t.Errorf("%s: took %s", c.mode, seen)
		}
	}
	s, _ := local(t, "other")
	if _, err := callLocal(t, s, 10*time.Second); err == nil || !strings.Contains(err.Error(), "2099-01-01") {
		t.Fatalf("a server of another modern version: %v", err)
	}
	s, _ = local(t, "noid")
	if _, err := callLocal(t, s, 10*time.Second); err == nil || !strings.Contains(err.Error(), "speaks only modern versions asz does not") {
		t.Fatalf("a refusal with no id: %v", err)
	}
}

// A modern request carries the version, who asks and no capability in its
// metadata.
func TestAModernLocalRequestCarriesItsMetadata(t *testing.T) {
	s, log := local(t, "modern")
	if _, err := callLocal(t, s, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	call := strings.Split(strings.TrimSpace(string(b)), "\n")[1]
	var m struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	_ = json.Unmarshal([]byte(call), &m)
	if m.Params.Meta["io.modelcontextprotocol/protocolVersion"] != Modern || m.Params.Meta["io.modelcontextprotocol/clientInfo"] == nil {
		t.Fatalf("metadata %v", m.Params.Meta)
	}
	if caps, _ := m.Params.Meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any); caps == nil || len(caps) != 0 {
		t.Fatalf("capabilities %v", m.Params.Meta)
	}
}

// A legacy server's ping is answered and any other request it sends is
// refused as a method asz does not offer; a modern server sends none, so
// one ends the fetch.
func TestALocalServersRequestIsAnsweredByEra(t *testing.T) {
	s, log := local(t, "legacy+requests")
	if _, err := callLocal(t, s, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if seen := strings.Join(took(t, log), " "); !strings.Contains(seen, `tools/call answered "r1" refused "r2" -32601`) {
		t.Fatalf("took %s", seen)
	}
	s, _ = local(t, "modern+requests")
	if _, err := callLocal(t, s, 10*time.Second); err == nil || !strings.Contains(err.Error(), "a modern server sent a request") {
		t.Fatalf("modern: %v", err)
	}
}

// A call that runs out of time is cancelled with a notification naming it;
// a handshake that runs out of time is not, since a legacy client must not
// cancel it.
func TestALocalCallOutOfTimeIsCancelled(t *testing.T) {
	for _, c := range []struct {
		mode, last string
	}{
		{"modern+slow", "notifications/cancelled"},
		{"legacy+slowinit", "initialize"},
	} {
		s, log := local(t, c.mode)
		_, err := callLocal(t, s, time.Second)
		var e *Error
		if !errors.As(err, &e) || e.Class != ClassTimeout {
			t.Fatalf("%s: %v", c.mode, err)
		}
		seen := took(t, log)
		if seen[len(seen)-1] != c.last {
			t.Errorf("%s: took %v", c.mode, seen)
		}
	}
}

// What does not hold ends the fetch: a line past the bound, a server that
// ends before its answer.
func TestALocalServerThatBreaksEndsTheFetch(t *testing.T) {
	for _, c := range []struct {
		mode, class string
	}{
		{"modern+big", ClassTooLarge},
		{"modern+exit", ClassTransport},
	} {
		s, _ := local(t, c.mode)
		s.MaxMessage = 1024
		_, err := callLocal(t, s, 10*time.Second)
		var e *Error
		if !errors.As(err, &e) || e.Class != c.class {
			t.Errorf("%s: %v", c.mode, err)
		}
	}
}

// A server that stops reading its input cannot hold a write past the
// fetch's time.
func TestALocalServerThatStopsReadingIsEnded(t *testing.T) {
	s, _ := local(t, "modern+deaf")
	start := time.Now()
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_, err := Call(ctx, s, "get", map[string]any{"key": strings.Repeat("k", 1<<20)})
	var e *Error
	if !errors.As(err, &e) || e.Class != ClassTimeout || time.Since(start) > 6*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

// What a server wrote before it ended is read whole, however late it is
// read: its output is never closed under the reader when the server is
// reaped.
func TestAServersOutputOutlivesIt(t *testing.T) {
	s, _ := local(t, "flood")
	x, err := open(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	ex := x.(*stdioExchange)
	defer ex.close()
	select {
	case <-ex.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not end")
	}
	lines := 0
	for l := range ex.lines {
		if l.err != nil {
			break
		}
		lines++
	}
	if lines != 30 {
		t.Fatalf("%d lines of 30", lines)
	}
}

// A server is ended however it behaves: one whose child keeps its pipes
// open, and one that stays after its input closes, each within seconds.
func TestALocalServerIsEnded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the child is started with sleep, which Windows does not have")
	}
	for _, mode := range []string{"modern+child", "modern+stubborn"} {
		s, _ := local(t, mode)
		s.Stderr = &strings.Builder{}
		start := time.Now()
		if _, err := callLocal(t, s, 20*time.Second); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if d := time.Since(start); d > 8*time.Second {
			t.Errorf("%s: ended after %v", mode, d)
		}
	}
}

// A list follows its pages, an empty cursor included.
func TestALocalListFollowsItsPages(t *testing.T) {
	s, _ := local(t, "legacy+paged")
	l, err := List(context.Background(), s, 5)
	if err != nil || len(l.Tools) != 2 || !l.Complete || l.Era != EraLegacy {
		t.Fatalf("%+v %v", l, err)
	}
}
