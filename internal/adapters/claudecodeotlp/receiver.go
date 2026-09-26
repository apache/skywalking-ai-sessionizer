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

// Package claudecodeotlp is the claude-code-otlp adapter: an OpenTelemetry
// receiver Claude Code's own exporter can be pointed at. It accepts metrics,
// logs and traces, counts each request so the status says what the runtime
// sends, and keeps none of them. A runtime configured with an exporter keeps
// working, and nothing it sends is counted twice.
//
// asz does not land the exporter's metrics. Every metric asz sends is derived
// from the landed files, under one name for every runtime, so one root has
// one source for each count.
package claudecodeotlp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

const (
	// Name is the adapter's name in the configuration.
	Name = "claude-code-otlp"
	// Version is the adapter's contract version.
	Version = "0.1.0"
	// MaxRequest bounds one request, well above what the runtime's exporter
	// sends in a minute.
	MaxRequest = 64 << 20
)

// Stats counts the requests accepted and dropped since the receiver started.
type Stats struct {
	Metrics int64
	Logs    int64
	Traces  int64
}

// Receiver listens for the runtime's exporter on one address, over gRPC
// and over HTTP with protobuf, the two protocols the runtime offers.
type Receiver struct {
	// Listen is the address, such as 127.0.0.1:4317. Both protocols share
	// it: gRPC and HTTP/1.1 are told apart on the first bytes.
	Listen string

	metrics atomic.Int64
	logs    atomic.Int64
	traces  atomic.Int64

	mu   sync.Mutex
	ln   net.Listener
	rpc  *grpc.Server
	web  *http.Server
	done chan struct{}
}

// Stats reports the counts so far.
func (r *Receiver) Stats() Stats {
	return Stats{Metrics: r.metrics.Load(), Logs: r.logs.Load(), Traces: r.traces.Load()}
}

// Addr is the address the receiver listens on, once started.
func (r *Receiver) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return ""
	}
	return r.ln.Addr().String()
}

// Start opens the listener and serves until Stop. The one port serves
// both protocols: a connection that opens with an HTTP/2 preface is gRPC,
// anything else is HTTP/1.1.
func (r *Receiver) Start() error {
	if r.Listen == "" {
		return errors.New("claude-code-otlp: no listen address")
	}
	ln, err := net.Listen("tcp", r.Listen)
	if err != nil {
		return fmt.Errorf("claude-code-otlp: %w", err)
	}
	r.rpc = grpc.NewServer(grpc.MaxRecvMsgSize(MaxRequest))
	collmetricspb.RegisterMetricsServiceServer(r.rpc, &metricsServer{r: r})
	collogspb.RegisterLogsServiceServer(r.rpc, &logsServer{r: r})
	colltracepb.RegisterTraceServiceServer(r.rpc, &traceServer{r: r})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/metrics", r.serveDropped(&r.metrics))
	mux.HandleFunc("/v1/logs", r.serveDropped(&r.logs))
	mux.HandleFunc("/v1/traces", r.serveDropped(&r.traces))
	r.web = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	r.mu.Lock()
	r.ln = ln
	r.done = make(chan struct{})
	r.mu.Unlock()
	go r.serve(ln)
	return nil
}

// serve splits the one listener by protocol. gRPC needs HTTP/2, which
// starts with a fixed preface; an HTTP/1.1 request starts with a method.
func (r *Receiver) serve(ln net.Listener) {
	defer close(r.done)
	grpcLn := &pipeListener{addr: ln.Addr(), conns: make(chan net.Conn), closed: make(chan struct{})}
	webLn := &pipeListener{addr: ln.Addr(), conns: make(chan net.Conn), closed: make(chan struct{})}
	go func() { _ = r.rpc.Serve(grpcLn) }()
	go func() { _ = r.web.Serve(webLn) }()
	for {
		c, err := ln.Accept()
		if err != nil {
			close(grpcLn.closed)
			close(webLn.closed)
			return
		}
		go func(c net.Conn) {
			pc := &peekConn{Conn: c}
			if pc.isHTTP2() {
				grpcLn.conns <- pc
			} else {
				webLn.conns <- pc
			}
		}(c)
	}
}

// Stop closes the listener and waits for the servers to finish.
func (r *Receiver) Stop() {
	r.mu.Lock()
	ln, done := r.ln, r.done
	r.mu.Unlock()
	if ln == nil {
		return
	}
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.web.Shutdown(ctx)
	r.rpc.Stop()
	<-done
}

// serveDropped accepts a request and keeps nothing of it, counting it so the
// status can say what the runtime is sending. The answer is an empty export
// response in the request's own encoding, as OTLP asks: no bytes in
// protobuf, and {} in JSON.
func (r *Receiver) serveDropped(counter *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, MaxRequest))
		counter.Add(1)
		if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}
}

type metricsServer struct {
	collmetricspb.UnimplementedMetricsServiceServer
	r *Receiver
}

func (s *metricsServer) Export(context.Context, *collmetricspb.ExportMetricsServiceRequest) (*collmetricspb.ExportMetricsServiceResponse, error) {
	s.r.metrics.Add(1)
	return &collmetricspb.ExportMetricsServiceResponse{}, nil
}

type logsServer struct {
	collogspb.UnimplementedLogsServiceServer
	r *Receiver
}

func (s *logsServer) Export(context.Context, *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.r.logs.Add(1)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

type traceServer struct {
	colltracepb.UnimplementedTraceServiceServer
	r *Receiver
}

func (s *traceServer) Export(context.Context, *colltracepb.ExportTraceServiceRequest) (*colltracepb.ExportTraceServiceResponse, error) {
	s.r.traces.Add(1)
	return &colltracepb.ExportTraceServiceResponse{}, nil
}

// pipeListener hands the connections one protocol owns to its server.
type pipeListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { return nil }
func (l *pipeListener) Addr() net.Addr { return l.addr }

// peekConn reads the first bytes of a connection to tell the protocol and
// gives them back to whoever reads next.
type peekConn struct {
	net.Conn
	head []byte
}

const h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

func (c *peekConn) isHTTP2() bool {
	buf := make([]byte, len(h2Preface))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _ := io.ReadFull(c.Conn, buf)
	_ = c.SetReadDeadline(time.Time{})
	c.head = buf[:n]
	return string(c.head) == h2Preface
}

func (c *peekConn) Read(p []byte) (int, error) {
	if len(c.head) > 0 {
		n := copy(p, c.head)
		c.head = c.head[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
