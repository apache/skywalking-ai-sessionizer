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

package metrics_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// A new capture of a runtime's exporter enters testdata through this test,
// so it can never be committed with a value that names someone:
//
//	go test ./internal/metrics -run TestCaptures -capture "$PWD/FILE.pb" -capture-name claude-code-VERSION-metrics
//
// FILE.pb is one spooled ExportMetricsServiceRequest. The test writes
// testdata/NAME.pb with the identifying values replaced, and testdata/NAME.json,
// the same request in the protocol's JSON, for a person to read.
var (
	captureFile = flag.String("capture", "", "a spooled OTLP metrics request to redact into testdata")
	captureName = flag.String("capture-name", "", "the name of the capture in testdata, with no extension")
)

// redacted names the attributes whose values identify a person, an account,
// an organisation or a session. Their values are replaced and their presence
// is kept, because the parity test must see that the exporter sends them.
var redacted = []string{"user.", "organization.", "session.id", "prompt.id", "host.name", "service.instance.id"}

func identifying(key string) bool {
	for _, prefix := range redacted {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// attributes calls fn with every attribute list a request carries.
func attributes(req *collmetricspb.ExportMetricsServiceRequest, fn func([]*commonpb.KeyValue)) {
	for _, rm := range req.ResourceMetrics {
		fn(rm.GetResource().GetAttributes())
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, dp := range m.GetSum().GetDataPoints() {
					fn(dp.Attributes)
				}
				for _, dp := range m.GetGauge().GetDataPoints() {
					fn(dp.Attributes)
				}
				for _, dp := range m.GetHistogram().GetDataPoints() {
					fn(dp.Attributes)
				}
			}
		}
	}
}

func readRequest(t *testing.T, path string) *collmetricspb.ExportMetricsServiceRequest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var req collmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		t.Fatalf("%s is not an ExportMetricsServiceRequest: %v", path, err)
	}
	return &req
}

func TestCaptures(t *testing.T) {
	if *captureFile != "" {
		if *captureName == "" || strings.ContainsAny(*captureName, "/\\.") {
			t.Fatal("-capture needs -capture-name, a file name with no directory and no extension")
		}
		req := readRequest(t, *captureFile)
		attributes(req, func(kvs []*commonpb.KeyValue) {
			for _, kv := range kvs {
				if identifying(kv.GetKey()) {
					kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "redacted"}}
				}
			}
		})
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		text, err := protojson.MarshalOptions{Multiline: true, Indent: " "}.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Join("testdata", *captureName)
		if err := os.WriteFile(base+".pb", data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(base+".json", append(text, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s.pb and %s.json", base, base)
	}

	captures, err := filepath.Glob(filepath.Join("testdata", "*.pb"))
	if err != nil || len(captures) == 0 {
		t.Fatalf("no capture in testdata: %v", err)
	}
	for _, path := range captures {
		req := readRequest(t, path)
		attributes(req, func(kvs []*commonpb.KeyValue) {
			for _, kv := range kvs {
				if identifying(kv.GetKey()) && kv.GetValue().GetStringValue() != "redacted" {
					t.Errorf("%s holds a value for %s. Write captures with -capture, which replaces it", path, kv.GetKey())
				}
			}
		})
		// protojson varies its spacing on purpose, so the two are compared
		// as requests, not as bytes.
		text, err := os.ReadFile(strings.TrimSuffix(path, ".pb") + ".json")
		if err != nil {
			t.Fatalf("%s has no JSON beside it: %v", path, err)
		}
		var fromJSON collmetricspb.ExportMetricsServiceRequest
		if err := protojson.Unmarshal(text, &fromJSON); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(req, &fromJSON) {
			t.Errorf("%s is not the request %s holds", strings.TrimSuffix(path, ".pb")+".json", path)
		}
	}
}
