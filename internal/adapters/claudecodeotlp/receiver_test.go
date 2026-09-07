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
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
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

// The whole path of the runtime's own metrics: sent by the exporter to the
// receiver, landed in the spool, sent on by asz push under asz's identity
// with the points untouched, and sent once.
func TestReceivedMetricsReachTheReceiverOfThePush(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	rcv := &claudecodeotlp.Receiver{Zone: z, Listen: "127.0.0.1:0", LandMetrics: true}
	if err := rcv.Start(); err != nil {
		t.Fatal(err)
	}
	defer rcv.Stop()
	runtime, err := otlp.NewClient(otlp.Options{Protocol: otlp.ProtocolGRPC, Endpoint: rcv.Addr(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.ExportMetrics(request()); err != nil {
		t.Fatal(err)
	}

	oap, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer oap.Close()
	for _, protocol := range []string{otlp.ProtocolGRPC, otlp.ProtocolHTTP} {
		client, err := oap.Client(protocol)
		if err != nil {
			t.Fatal(err)
		}
		p := &otlp.Pusher{Zone: z, Client: client, Version: "test", ServiceName: "Claude Code", InstanceID: "sender-1", Layer: "AI_AGENT"}
		st, err := p.Pass()
		if err != nil || len(st.Errors) != 0 {
			t.Fatal(err, st.Errors)
		}
		_ = client.Close()
		got := oap.MetricsRequests()
		if protocol == otlp.ProtocolGRPC {
			if st.Metrics != 1 || len(got) != 1 {
				t.Fatalf("%s: sent %d, the receiver holds %d", protocol, st.Metrics, len(got))
			}
			res := otlptest.Attrs(got[0].GetResourceMetrics()[0].GetResource().GetAttributes())
			if res["service.name"] != "Claude Code" || res["service.instance.id"] != "sender-1" || res["service.layer"] != "AI_AGENT" {
				t.Fatalf("resource on the wire: %v", res)
			}
			m := got[0].GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0]
			if m.GetName() != "claude_code.token.usage" || m.GetSum().GetDataPoints()[0].GetAsInt() != 42 {
				t.Fatalf("the runtime's points did not travel unchanged: %v", m)
			}
		} else if st.Metrics != 0 || len(got) != 1 {
			t.Fatalf("a second pass over the other transport sent the request again: %d, %d", st.Metrics, len(got))
		}
	}
}
