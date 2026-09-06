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

// Package otlptest is an in-process OTLP logs receiver for tests. It serves
// both transports at once, keeps every request it accepted, and can be told
// to refuse them. The scenario checks and the exporter's own tests use it,
// so the same receiver reads what a pusher sent over either transport.
package otlptest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
)

// Receiver listens on both transports and keeps what it accepted.
type Receiver struct {
	mu   sync.Mutex
	reqs []*collogspb.ExportLogsServiceRequest
	fail bool

	web  *httptest.Server
	rpc  *grpc.Server
	addr string
}

// Start opens a receiver on two loopback ports, one per transport.
func Start() (*Receiver, error) {
	r := &Receiver{}
	r.web = httptest.NewServer(http.HandlerFunc(r.serveHTTP))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.web.Close()
		return nil, err
	}
	r.rpc = grpc.NewServer(grpc.MaxRecvMsgSize(64 << 20))
	collogspb.RegisterLogsServiceServer(r.rpc, &logsServer{r: r})
	go func() { _ = r.rpc.Serve(lis) }()
	r.addr = lis.Addr().String()
	return r, nil
}

// Options is how a pusher reaches the receiver over the transport named.
func (r *Receiver) Options(protocol string) otlp.Options {
	if protocol == otlp.ProtocolHTTP {
		return otlp.Options{Protocol: protocol, Endpoint: r.web.URL}
	}
	return otlp.Options{Protocol: otlp.ProtocolGRPC, Endpoint: r.addr}
}

// Client opens a client for the transport named.
func (r *Receiver) Client(protocol string) (otlp.Client, error) {
	return otlp.NewClient(r.Options(protocol))
}

// Requests is every request accepted so far, in order.
func (r *Receiver) Requests() []*collogspb.ExportLogsServiceRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*collogspb.ExportLogsServiceRequest(nil), r.reqs...)
}

// Reset forgets every request accepted so far.
func (r *Receiver) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = nil
}

// Fail makes the receiver refuse every request, or accept them again.
func (r *Receiver) Fail(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fail
}

// Close stops both listeners.
func (r *Receiver) Close() {
	r.web.Close()
	r.rpc.Stop()
}

func (r *Receiver) accept(req *collogspb.ExportLogsServiceRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("down")
	}
	r.reqs = append(r.reqs, req)
	return nil
}

// serveHTTP is what an OTLP/HTTP receiver expects: a protobuf body posted
// to /v1/logs, answered with a protobuf response.
func (r *Receiver) serveHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/v1/logs" || req.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "wrong path or content type", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var msg collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &msg); err != nil {
		http.Error(w, "not an ExportLogsServiceRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.accept(&msg); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	out, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

type logsServer struct {
	collogspb.UnimplementedLogsServiceServer
	r *Receiver
}

func (s *logsServer) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if err := s.r.accept(req); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// Attrs flattens attributes for a check. An integer is written as int:N so
// a check can tell the two kinds the exporter sends apart.
func Attrs(kvs []*commonpb.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_IntValue:
			m[kv.GetKey()] = fmt.Sprintf("int:%d", v.IntValue)
		default:
			m[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
	}
	return m
}

// Records is every log record of every request, in order.
func Records(reqs []*collogspb.ExportLogsServiceRequest) []*logspb.LogRecord {
	var out []*logspb.LogRecord
	for _, req := range reqs {
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				out = append(out, sl.GetLogRecords()...)
			}
		}
	}
	return out
}
