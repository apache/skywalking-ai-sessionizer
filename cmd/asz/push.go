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
func cmdPush(cfg *config.Config, _ config.Adapter, _ bool) error {
	o := cfg.Export.OTLP
	if o.Endpoint == "" {
		return fmt.Errorf("push: set export.otlp.endpoint: the receiver's gRPC address, for example 127.0.0.1:11800, or with protocol http its base URL, for example http://127.0.0.1:12800")
	}
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
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
		if len(st.Errors) > 0 {
			return st, fmt.Errorf("pass incomplete: %d error(s); unsent files are retried on the next pass", len(st.Errors))
		}
		if st.Deferred > 0 {
			// Rounds another process was still writing. This command makes
			// one pass, so nothing comes back for them on its own.
			return st, fmt.Errorf("pass incomplete: %d round(s) were still being written and were not sent; run it again", st.Deferred)
		}
		return st, nil
	}
	if _, err = pass(); errors.Is(err, storage.ErrExportBusy) {
		return fmt.Errorf("another pass over this root held the export state for %s; nothing was sent, run it again", exportWait)
	}
	return err
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
		Zone:        storage.NewZone(zoneRoot),
		Client:      client,
		Version:     version,
		ServiceName: o.ServiceName,
		Runtimes:    map[string]string{claudecode.Name: claudecode.RuntimeName, claudecodechanges.Name: claudecodechanges.RuntimeName, mock.Name: mock.RuntimeName},
		InstanceID:  o.InstanceID,
		Layer:       o.Layer,
		BatchBytes:  o.BatchBytes,

		MaxBytesPerMinute: o.MaxBytesPerMinute,
		NoLogs:            !o.SendLogs(),
		NoMetrics:         !o.SendMetrics(),
		// The metrics spool concerns Claude Code whichever adapter filled it,
		// the local derivation or the runtime's own exporter.
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
