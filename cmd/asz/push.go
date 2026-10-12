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

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/kiro"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/mock"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// cmdPush sends the storage root's landed files and rounds to an
// OpenTelemetry logs receiver, in one pass.
//
// One pass is all it does. A root that keeps growing is sent by asz collect,
// which pushes what it has landed at the end of every period; this command
// is for sending a root that is already there, such as one copied from
// another machine or brought under a new budget by asz repack.
//
// Metrics are derived only when they will be sent, so a root collected with
// no endpoint holds none. When this command sends metrics, it first derives
// what the root has not derived yet, as a pass of asz collect does.
func cmdPush(cfg *config.Config, _ config.Adapter, _ bool) error {
	o := cfg.Export.OTLP
	if o.Endpoint == "" {
		return fmt.Errorf("push: set export.otlp.endpoint: the receiver's gRPC address, for example 127.0.0.1:11800, or with protocol http its base URL, for example http://127.0.0.1:12800")
	}
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	lock, err := holdRootFor(zoneRoot, "push")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	p, closeClient, err := newPusher(cfg, zoneRoot)
	if err != nil {
		return err
	}
	defer closeClient()
	// This command was asked to send, so it waits for a collect or a server
	// over the same root rather than reporting success having sent nothing.
	p.WaitForExport = exportWait
	service := o.ServiceName
	if service == "" {
		service = "the runtime of each session (Claude Code, Mock Agent)"
	}
	endpoint := o.Endpoint + " over gRPC"
	if o.Protocol == otlp.ProtocolHTTP {
		endpoint = o.Endpoint + "/v1/logs over HTTP"
	}
	rate := "no limit"
	if o.MaxBytesPerMinute > 0 {
		rate = humanBytes(o.MaxBytesPerMinute) + " per minute"
	}
	var sending []string
	if o.SendLogs() {
		sending = append(sending, "landed files and rounds, as logs")
	}
	if o.SendMetrics() {
		sending = append(sending, "the metrics spool")
	}
	fmt.Printf("storage root: %s\nendpoint    : %s\nservice     : %s\ninstance    : %s\nlayer       : %s\nrate        : %s\nsending     : %s\n",
		zoneRoot, endpoint, service, p.InstanceID, o.Layer, rate, strings.Join(sending, "; "))
	// The derivation runs inside the pass, under the export lock, and a file
	// it could not derive fails the command: its metrics were not sent.
	var derived []error
	if cfg.MetricsSent() {
		d, err := newDeriver(storage.NewZone(zoneRoot), cfg.Metrics, true)
		if err != nil {
			return err
		}
		p.BeforeSend = func() error {
			ms, err := d.Pass(nil)
			if err != nil {
				return fmt.Errorf("push: derive the metrics: %w", err)
			}
			fmt.Printf("derived     : %d file(s), %d request(s), %d point(s)\n", ms.Files, ms.Requests, ms.Points)
			for _, e := range ms.Errors {
				fmt.Fprintf(os.Stderr, "  error: %v\n", e)
			}
			derived = ms.Errors
			return nil
		}
	}
	pass := func() (*otlp.Stats, error) {
		start := time.Now()
		st, err := p.Pass()
		if err != nil {
			return nil, err
		}
		fmt.Printf("[%s] files=%d metrics=%d bytes=%s wire=%s requests=%d paused=%s rejected=%d errors=%d (%s)\n",
			time.Now().Format("15:04:05"), st.Files, st.Metrics, humanBytes(st.Bytes), humanBytes(st.Wire), st.Requests,
			st.Paused.Round(time.Second), st.Rejected, len(st.Errors), time.Since(start).Round(time.Millisecond))
		for _, e := range st.Errors {
			fmt.Fprintf(os.Stderr, "  error: %v\n", e)
		}
		for _, h := range st.Held {
			fmt.Fprintf(os.Stderr, "  held: %v\n", heldError(h))
		}
		if len(st.Errors) > 0 {
			return st, fmt.Errorf("pass incomplete: %d error(s); unsent files are retried on the next pass", len(st.Errors))
		}
		if st.Deferred > 0 {
			// Rounds another process was still writing. This command makes
			// one pass, so nothing comes back for them on its own.
			return st, fmt.Errorf("pass incomplete: %d round(s) were still being written and were not sent; run it again", st.Deferred)
		}
		if len(st.Held) > 0 {
			return st, fmt.Errorf("pass incomplete: %d session(s) were sent before under another receiver, service or instance, and were held", len(st.Held))
		}
		if len(derived) > 0 {
			return st, fmt.Errorf("pass incomplete: the metrics of %d landed file(s) could not be derived and were not sent; run it again", len(derived))
		}
		return st, nil
	}
	if _, err = pass(); errors.Is(err, storage.ErrExportBusy) {
		return fmt.Errorf("another pass over this root held the export state for %s; nothing was sent, run it again", exportWait)
	}
	return err
}

// heldError says why a session was not sent: it went before to another
// owner, and its later files go there or nowhere.
func heldError(h otlp.Held) error {
	return fmt.Errorf("session %s was sent to %s and this pass sends to %s; its later files wait, since a receiver holds a session whole. Set the endpoint, service and instance back to send them",
		h.Session, h.Pinned, h.Now)
}

// exportWait is how long a command that was asked to send waits for another
// pass over the same root. Long enough to outlast an ordinary pass, short
// enough that a stuck one is reported rather than waited on for ever.
const exportWait = 2 * time.Minute

// newPusher builds the pusher the configured receiver needs, and the
// function that closes its connection. It returns a nil pusher when no
// endpoint is named, so a caller that pushes only when it is asked to, such
// as the pipeline asz collect runs, needs no test of its own.
func newPusher(cfg *config.Config, zoneRoot string) (*otlp.Pusher, func(), error) {
	o := cfg.Export.OTLP
	if o.Endpoint == "" {
		return nil, func() {}, nil
	}
	client, err := otlp.NewClient(otlp.Options{Protocol: o.Protocol, Endpoint: o.Endpoint, Headers: o.Headers, TLS: o.TLS})
	if err != nil {
		return nil, func() {}, fmt.Errorf("push: %w", err)
	}
	// The service is the configured name, or else the runtime that produced
	// each session, read off its landed headers: one service per kind of
	// agent, which is how a receiver lists them.
	p := &otlp.Pusher{
		Zone:   storage.NewZone(zoneRoot),
		Client: client,
		// Recorded beside what is sent, so a scenario root removes a session
		// only once it is sent to the receiver this pipeline sends to.
		Endpoint:    otlp.EndpointOf(o.Protocol, o.Endpoint, o.TLS),
		Version:     version,
		ServiceName: o.ServiceName,
		Runtimes: map[string]string{claudecode.Name: claudecode.RuntimeName, claudecodechanges.Name: claudecodechanges.RuntimeName,
			claudecodechanges.NameWas: claudecodechanges.RuntimeName,
			langsmith.Name:            langsmith.RuntimeName, claudecodeprovider.Name: claudecodeprovider.RuntimeName, mock.Name: mock.RuntimeName,
			kiro.Name: kiro.RuntimeName},
		// A session whose transcript has not landed yet is named by asz
		// itself when it came through the receiver, and that name says so.
		SessionRuntime: func(session string) string {
			if langsmith.IsSessionID(session) {
				return langsmith.RuntimeName
			}
			return ""
		},
		InstanceID: o.InstanceID,
		Layer:      o.Layer,
		BatchBytes: o.BatchBytes,

		MaxBytesPerMinute: o.MaxBytesPerMinute,
		NoLogs:            !o.SendLogs(),
		NoMetrics:         !o.SendMetrics(),
		// The metrics spool concerns Claude Code: everything in it is derived
		// from Claude Code's landed files.
		MetricsService: claudecode.RuntimeName,
	}
	if o.ServiceName != "" {
		p.MetricsService = o.ServiceName
	}
	if err := p.Prepare(); err != nil {
		client.Close()
		return nil, func() {}, err
	}
	return p, func() { client.Close() }, nil
}
