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

package claudecodeotlp_test

import (
	"bytes"
	"net/http"
	"os"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeotlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func request() *collmetricspb.ExportMetricsServiceRequest {
	return &collmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-code"}}},
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "claude_code.token.usage", Unit: "tokens",
			Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: true, DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: 1, Value: &metricspb.NumberDataPoint_AsInt{AsInt: 42}}}}}}}}},
	}}}
}

// The receiver takes the runtime's metrics over either protocol on one
// port and lands each request in the spool, bytes as sent; logs are
// accepted, counted and dropped.
func TestReceiverLandsMetricsFromBothProtocols(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	rcv := &claudecodeotlp.Receiver{Zone: z, Listen: "127.0.0.1:0", LandMetrics: true, Now: func() time.Time { return time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) }}
	if err := rcv.Start(); err != nil {
		t.Fatal(err)
	}
	defer rcv.Stop()
	addr := rcv.Addr()
	want, _ := proto.Marshal(request())

	for _, o := range []otlp.Options{
		{Protocol: otlp.ProtocolGRPC, Endpoint: addr, Timeout: 10 * time.Second},
		{Protocol: otlp.ProtocolHTTP, Endpoint: "http://" + addr, Timeout: 10 * time.Second},
	} {
		c, err := otlp.NewClient(o)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.ExportMetrics(request()); err != nil {
			t.Fatalf("%s: %v", o.Protocol, err)
		}
		if err := c.Export(&collogspb.ExportLogsServiceRequest{}); err != nil {
			t.Fatalf("%s: a logs request must be accepted and dropped: %v", o.Protocol, err)
		}
		_ = c.Close()
	}
	files, err := storage.NewSpool(z).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("the spool holds %d files, want one per metrics request", len(files))
	}
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want) || f.Source != claudecodeotlp.SourceOTLP {
			t.Fatalf("%s: not the request as sent, or not marked as the runtime's", f.Path)
		}
		if fi, _ := os.Stat(f.Path); fi.Mode().Perm()&0o222 != 0 {
			t.Fatalf("%s is writable; a spooled request is write-once", f.Path)
		}
	}
	st := rcv.Stats()
	if st.Metrics != 2 || st.Logs != 2 || st.Traces != 0 || st.Bytes != int64(2*len(want)) || st.Errors != 0 {
		t.Fatalf("stats %+v", st)
	}

	// A JSON body is refused with a word about the protocol to set.
	resp, err := http.Post("http://"+addr+"/v1/metrics", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("a JSON request answered %d", resp.StatusCode)
	}
}

// With metrics off the receiver accepts the runtime's metrics and keeps
// nothing, so a root whose local adapter derives them counts them once.
func TestReceiverWithMetricsOffDropsThem(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	rcv := &claudecodeotlp.Receiver{Zone: z, Listen: "127.0.0.1:0"}
	if err := rcv.Start(); err != nil {
		t.Fatal(err)
	}
	defer rcv.Stop()
	c, err := otlp.NewClient(otlp.Options{Protocol: otlp.ProtocolGRPC, Endpoint: rcv.Addr(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.ExportMetrics(request()); err != nil {
		t.Fatal(err)
	}
	if files, _ := storage.NewSpool(z).List(); len(files) != 0 {
		t.Fatalf("the spool holds %d files; metrics off must land nothing", len(files))
	}
	if st := rcv.Stats(); st.Metrics != 0 || st.MetricsDropped != 1 {
		t.Fatalf("stats %+v", st)
	}
}
