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
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// A pipeline derives metrics only when they will be sent. With no endpoint,
// or with export.otlp.metrics off, every request a pass derived stayed in
// the spool, which only grew.
func TestMetricsAreDerivedOnlyWhenSent(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name     string
		endpoint string
		send     *bool
		enabled  *bool
		derived  bool
	}{
		{"no endpoint", "", nil, nil, false},
		{"an endpoint", "127.0.0.1:11800", nil, nil, true},
		{"metrics not sent", "127.0.0.1:11800", &off, nil, false},
		{"metrics off", "127.0.0.1:11800", nil, &off, false},
	} {
		cfg := &config.Config{}
		cfg.Export.OTLP.Endpoint, cfg.Export.OTLP.Metrics, cfg.Metrics.Enabled = tc.endpoint, tc.send, tc.enabled
		if got := pipelineMetrics(cfg).On(); got != tc.derived {
			t.Errorf("%s: derives %v, want %v", tc.name, got, tc.derived)
		}
		if cfg.MetricsSent() != tc.derived {
			t.Errorf("%s: MetricsSent says %v, want %v", tc.name, cfg.MetricsSent(), tc.derived)
		}
	}
}

// A root collected with no endpoint holds no metrics, since nothing would
// have sent them. asz push, when it sends metrics, derives them first, so
// such a root sent later still carries its metrics.
func TestPushDerivesWhatACollectionWithNoEndpointLeft(t *testing.T) {
	root, _ := scenarioRoot(t, "mcp-calls.yaml")
	zone := storage.NewZone(root)
	local := &config.Config{Metrics: allMetrics}
	ref, err := newRefresher(view.New(zone, nil), zone, bothAdapters(root, ""), pipelineMetrics(local), 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	if files, err := storage.NewSpool(zone).List(); err != nil || len(files) != 0 {
		t.Fatalf("a collection with no endpoint spooled %d request(s), %v", len(files), err)
	}

	rcv := receiver(t)
	cfg := &config.Config{Metrics: allMetrics}
	cfg.Storage.Root = root
	o := rcv.Options(otlp.ProtocolGRPC)
	cfg.Export.OTLP.Protocol, cfg.Export.OTLP.Endpoint, cfg.Export.OTLP.ServiceName = o.Protocol, o.Endpoint, "Push Test"
	stdoutOf(t, func() {
		if err := cmdPush(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	})
	if n := len(rcv.MetricsRequests()); n == 0 {
		t.Fatal("asz push sent no metrics for a root collected with no endpoint")
	}
}

// A landed file push could not derive fails the command: its metrics were
// not sent. A receipt that does not match its landed file is one.
func TestPushFailsWhenAFileCannotBeDerived(t *testing.T) {
	root, _ := scenarioRoot(t, "mcp-calls.yaml")
	zone := storage.NewZone(root)
	ref, err := newRefresher(view.New(zone, nil), zone, bothAdapters(root, ""), pipelineMetrics(&config.Config{Metrics: allMetrics}), 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	sessions, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	planted := false
	for _, s := range sessions {
		files, _ := storage.LandedFiles(zone, s.Name())
		if len(files) == 0 {
			continue
		}
		dir := filepath.Join(zone.SessionDir(s.Name()), "metrics")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		bad := `{"schema":1,"digest":"not the landed file's"}`
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.json", files[0].Seq)), []byte(bad), 0o444); err != nil {
			t.Fatal(err)
		}
		planted = true
		break
	}
	if !planted {
		t.Fatal("no session landed")
	}
	rcv := receiver(t)
	cfg := &config.Config{Metrics: allMetrics}
	cfg.Storage.Root = root
	o := rcv.Options(otlp.ProtocolGRPC)
	cfg.Export.OTLP.Protocol, cfg.Export.OTLP.Endpoint, cfg.Export.OTLP.ServiceName = o.Protocol, o.Endpoint, "Push Test"
	var perr error
	stderrOf(t, func() { stdoutOf(t, func() { perr = cmdPush(cfg, config.Adapter{}, false) }) })
	if perr == nil || !strings.Contains(perr.Error(), "could not be derived") {
		t.Fatalf("push returned %v, want the files it could not derive reported", perr)
	}
}
