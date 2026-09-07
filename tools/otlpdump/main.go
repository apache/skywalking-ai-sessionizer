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

// Command otlpdump prints one spooled OTLP metrics request as JSON, the
// protocol's own JSON mapping, so what a runtime's exporter sent can be
// read and compared. With -redact the values of the attributes that name a
// person, an account, an organisation or a session are replaced, so a
// capture can be kept as a test fixture.
//
//	go run ./tools/otlpdump [-redact] FILE.pb
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Redacted names the attributes whose values identify a person, an account,
// an organisation or a session. Their values are replaced, their presence
// is kept: a parity test needs to see that the exporter sends them.
var Redacted = []string{"user.", "organization.", "session.id", "prompt.id", "host.name", "service.instance.id"}

func main() {
	redact := flag.Bool("redact", false, "replace the values of identifying attributes")
	out := flag.String("o", "", "write the (redacted) request as protobuf to this file")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: otlpdump [-redact] [-o FILE.pb] FILE.pb")
		os.Exit(2)
	}
	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var req collmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		fmt.Fprintln(os.Stderr, "not an ExportMetricsServiceRequest:", err)
		os.Exit(1)
	}
	if *redact {
		for _, rm := range req.ResourceMetrics {
			redactAttrs(rm.GetResource().GetAttributes())
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					for _, dp := range m.GetSum().GetDataPoints() {
						redactAttrs(dp.Attributes)
					}
					for _, dp := range m.GetGauge().GetDataPoints() {
						redactAttrs(dp.Attributes)
					}
					for _, dp := range m.GetHistogram().GetDataPoints() {
						redactAttrs(dp.Attributes)
					}
				}
			}
		}
	}
	if *out != "" {
		b, err := proto.Marshal(&req)
		if err == nil {
			err = os.WriteFile(*out, b, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	js, err := protojson.MarshalOptions{Multiline: true, Indent: " "}.Marshal(&req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(js))
}

func redactAttrs(kvs []*commonpb.KeyValue) {
	for _, kv := range kvs {
		for _, prefix := range Redacted {
			if strings.HasPrefix(kv.GetKey(), prefix) {
				kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "redacted"}}
			}
		}
	}
}
