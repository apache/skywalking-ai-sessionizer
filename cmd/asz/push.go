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
	"fmt"
	"os"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/mock"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// cmdPush sends the storage root's landed files and rounds to an
// OpenTelemetry logs receiver, once or on the export interval.
func cmdPush(cfg *config.Config, _ config.Adapter, once bool) error {
	o := cfg.Export.OTLP
	if o.Endpoint == "" {
		return fmt.Errorf("push: set export.otlp.endpoint: the receiver's gRPC address, for example 127.0.0.1:11800, or with protocol http its base URL, for example http://127.0.0.1:12800")
	}
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	client, err := otlp.NewClient(otlp.Options{Protocol: o.Protocol, Endpoint: o.Endpoint, Headers: o.Headers, TLS: o.TLS})
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer client.Close()
	// The service is the configured name, or else the runtime that produced
	// each session, read off its landed headers: one service per kind of
	// agent, which is how a receiver lists them.
	p := &otlp.Pusher{
		Zone:        storage.NewZone(zoneRoot),
		Client:      client,
		Version:     version,
		ServiceName: o.ServiceName,
		Runtimes:    map[string]string{claudecode.Name: claudecode.RuntimeName, mock.Name: mock.RuntimeName},
		InstanceID:  o.InstanceID,
		Layer:       o.Layer,
		BatchBytes:  o.BatchBytes,
	}
	if err := p.Prepare(); err != nil {
		return err
	}
	service := o.ServiceName
	if service == "" {
		service = "the runtime of each session (Claude Code, Mock Agent)"
	}
	endpoint := o.Endpoint + " over gRPC"
	if o.Protocol == otlp.ProtocolHTTP {
		endpoint = o.Endpoint + "/v1/logs over HTTP"
	}
	fmt.Printf("storage root: %s\nendpoint    : %s\nservice     : %s\ninstance    : %s\nlayer       : %s\n",
		zoneRoot, endpoint, service, p.InstanceID, o.Layer)
	pass := func() error {
		start := time.Now()
		st, err := p.Pass()
		if err != nil {
			return err
		}
		fmt.Printf("[%s] files=%d bytes=%s requests=%d errors=%d (%s)\n",
			time.Now().Format("15:04:05"), st.Files, humanBytes(st.Bytes), st.Requests,
			len(st.Errors), time.Since(start).Round(time.Millisecond))
		for _, e := range st.Errors {
			fmt.Fprintf(os.Stderr, "  error: %v\n", e)
		}
		if len(st.Errors) > 0 {
			return fmt.Errorf("pass incomplete: %d error(s); unsent files are retried on the next pass", len(st.Errors))
		}
		return nil
	}
	if once {
		return pass()
	}
	for {
		if err := pass(); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		time.Sleep(o.Interval)
	}
}
