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
	"io"
	"net/http"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeotlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
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

// The receiver takes what the runtime's exporter sends over either protocol
// on one port, answers each request as accepted, and counts it. It keeps
// nothing: asz derives every metric it sends from the landed files, so a
// runtime that also exports must not reach the spool.
func TestReceiverAcceptsAndDropsBothProtocols(t *testing.T) {
	rcv := &claudecodeotlp.Receiver{Listen: "127.0.0.1:0"}
	if err := rcv.Start(); err != nil {
		t.Fatal(err)
	}
	defer rcv.Stop()
	addr := rcv.Addr()

	for _, o := range []otlp.Options{
		{Protocol: otlp.ProtocolGRPC, Endpoint: addr, Timeout: 10 * time.Second},
		{Protocol: otlp.ProtocolHTTP, Endpoint: "http://" + addr, Timeout: 10 * time.Second},
	} {
		c, err := otlp.NewClient(o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExportMetrics(request()); err != nil {
			t.Fatalf("%s: a metrics request must be accepted and dropped: %v", o.Protocol, err)
		}
		if _, err := c.Export(&collogspb.ExportLogsServiceRequest{}); err != nil {
			t.Fatalf("%s: a logs request must be accepted and dropped: %v", o.Protocol, err)
		}
		_ = c.Close()
	}
	// A JSON body is accepted too: nothing is decoded, so its encoding does
	// not matter, and a runtime set to http/json keeps working. The answer is
	// in JSON, as OTLP asks.
	resp, err := http.Post("http://"+addr+"/v1/traces", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" || string(body) != "{}" {
		t.Fatalf("a JSON request answered %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if st := rcv.Stats(); st.Metrics != 2 || st.Logs != 2 || st.Traces != 1 {
		t.Fatalf("stats %+v", st)
	}
}
