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

package otlp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Throttled is a receiver asking the sender to slow down: 429 over HTTP,
// with the Retry-After it gives when it gives one, or ResourceExhausted
// over gRPC. A pass stops at it and leaves the rest for the next pass.
type Throttled struct {
	After  time.Duration
	Detail string
}

func (t *Throttled) Error() string {
	if t.After > 0 {
		return fmt.Sprintf("otlp: the receiver asked to slow down, retry after %s: %s", t.After, t.Detail)
	}
	return "otlp: the receiver asked to slow down: " + t.Detail
}

// The two transports OTLP defines for logs. Both carry the same request;
// only the connection differs. gRPC is the default: one long-lived HTTP/2
// connection carries every request of a pass, and the OAP's gRPC port
// accepts 50 MB a request where its HTTP port accepts 10 MiB.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http"
)

// Client delivers logs requests to one receiver.
type Client interface {
	// Export sends one request and returns an error unless the receiver
	// accepted every record in it. A partial success is an error too: the
	// receiver does not say which records it rejected, so the request is
	// sent again whole on the next pass, and a receiver keeps the first
	// copy of a file it already holds.
	Export(req *collogspb.ExportLogsServiceRequest) error
	// ExportMetrics sends one metrics request, with the same rules.
	ExportMetrics(req *collmetricspb.ExportMetricsServiceRequest) error
	Close() error
}

// Options describes the receiver.
type Options struct {
	// Protocol is grpc or http. Empty means grpc.
	Protocol string
	// Endpoint is host:port for grpc, such as 127.0.0.1:11800, and the
	// receiver's base URL for http, such as http://127.0.0.1:12800, to
	// which /v1/logs is appended.
	Endpoint string
	// Headers travel with every request, as gRPC metadata or as HTTP
	// headers: an authorization token, for example.
	Headers map[string]string
	// TLS makes the gRPC connection a TLS one, verified against the
	// system's roots. Over HTTP the scheme of the endpoint decides.
	TLS bool
	// Timeout bounds one request. The default is 60 seconds.
	Timeout time.Duration
}

// NewClient opens a client for the transport the options name. A gRPC
// client connects on the first request, so an unreachable receiver is
// reported by Export, not here.
func NewClient(o Options) (Client, error) {
	if o.Endpoint == "" {
		return nil, errors.New("otlp: no endpoint")
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	switch o.Protocol {
	case ProtocolGRPC, "":
		return newGRPC(o)
	case ProtocolHTTP:
		return newHTTP(o)
	}
	return nil, fmt.Errorf("otlp: unknown protocol %q, want grpc or http", o.Protocol)
}

// grpcClient calls the OTLP logs service. OTLP defines Export as a unary
// call, so a pass is a sequence of calls on one connection; there is no
// streaming call to use.
type grpcClient struct {
	conn    *grpc.ClientConn
	logs    collogspb.LogsServiceClient
	metrics collmetricspb.MetricsServiceClient
	headers map[string]string
	timeout time.Duration
}

func newGRPC(o Options) (Client, error) {
	if strings.Contains(o.Endpoint, "://") {
		return nil, fmt.Errorf("otlp: a grpc endpoint is host:port, not a URL: %s", o.Endpoint)
	}
	creds := insecure.NewCredentials()
	if o.TLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(o.Endpoint, grpc.WithTransportCredentials(creds), grpc.WithUserAgent("asz"))
	if err != nil {
		return nil, fmt.Errorf("otlp: %w", err)
	}
	return &grpcClient{conn: conn, logs: collogspb.NewLogsServiceClient(conn), metrics: collmetricspb.NewMetricsServiceClient(conn),
		headers: o.Headers, timeout: o.timeout()}, nil
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 60 * time.Second
	}
	return o.Timeout
}

func (c *grpcClient) context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	for k, v := range c.headers {
		ctx = metadata.AppendToOutgoingContext(ctx, k, v)
	}
	return ctx, cancel
}

func (c *grpcClient) failure(err error) error {
	if status.Code(err) == codes.ResourceExhausted {
		return &Throttled{Detail: c.conn.Target() + ": " + err.Error()}
	}
	return fmt.Errorf("otlp: %s: %w", c.conn.Target(), err)
}

func (c *grpcClient) Export(req *collogspb.ExportLogsServiceRequest) error {
	ctx, cancel := c.context()
	defer cancel()
	resp, err := c.logs.Export(ctx, req)
	if err != nil {
		return c.failure(err)
	}
	return rejected(resp)
}

func (c *grpcClient) ExportMetrics(req *collmetricspb.ExportMetricsServiceRequest) error {
	ctx, cancel := c.context()
	defer cancel()
	resp, err := c.metrics.Export(ctx, req)
	if err != nil {
		return c.failure(err)
	}
	if ps := resp.GetPartialSuccess(); ps.GetRejectedDataPoints() > 0 {
		return fmt.Errorf("otlp: the receiver rejected %d data point(s): %s", ps.GetRejectedDataPoints(), ps.GetErrorMessage())
	}
	return nil
}

func (c *grpcClient) Close() error { return c.conn.Close() }

// httpClient posts each request to the receiver's /v1/logs or /v1/metrics.
type httpClient struct {
	base    string
	headers map[string]string
	http    *http.Client
}

func newHTTP(o Options) (Client, error) {
	if !strings.HasPrefix(o.Endpoint, "http://") && !strings.HasPrefix(o.Endpoint, "https://") {
		return nil, fmt.Errorf("otlp: an http endpoint is a URL such as http://127.0.0.1:12800: %s", o.Endpoint)
	}
	return &httpClient{
		base:    strings.TrimRight(o.Endpoint, "/"),
		headers: o.Headers,
		http:    &http.Client{Timeout: o.timeout()},
	}, nil
}

func (c *httpClient) Export(req *collogspb.ExportLogsServiceRequest) error {
	answer, err := c.post(c.base+"/v1/logs", req)
	if err != nil {
		return err
	}
	var out collogspb.ExportLogsServiceResponse
	if len(answer) > 0 {
		if err := proto.Unmarshal(answer, &out); err != nil {
			return fmt.Errorf("otlp: %s answered with a body that is not an OTLP response: %w", c.base, err)
		}
	}
	return rejected(&out)
}

func (c *httpClient) ExportMetrics(req *collmetricspb.ExportMetricsServiceRequest) error {
	answer, err := c.post(c.base+"/v1/metrics", req)
	if err != nil {
		return err
	}
	var out collmetricspb.ExportMetricsServiceResponse
	if len(answer) > 0 {
		if err := proto.Unmarshal(answer, &out); err != nil {
			return fmt.Errorf("otlp: %s answered with a body that is not an OTLP response: %w", c.base, err)
		}
	}
	if ps := out.GetPartialSuccess(); ps.GetRejectedDataPoints() > 0 {
		return fmt.Errorf("otlp: the receiver rejected %d data point(s): %s", ps.GetRejectedDataPoints(), ps.GetErrorMessage())
	}
	return nil
}

// post sends one request as protobuf and returns the answer's body when it
// is an OTLP response, or nothing when the receiver answered with anything
// else, such as an empty body from a proxy, which is a full success as far
// as it says.
func (c *httpClient) post(url string, req proto.Message) ([]byte, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("otlp: %w", err)
	}
	hr, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	hr.Header.Set("User-Agent", "asz")
	for k, v := range c.headers {
		hr.Header.Set(k, v)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusTooManyRequests {
		t := &Throttled{Detail: url + " answered " + resp.Status}
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			t.After = time.Duration(secs) * time.Second
		}
		return nil, t
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := answer
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		return nil, fmt.Errorf("otlp: %s answered %s: %s", url, resp.Status, strings.TrimSpace(string(snippet)))
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-protobuf") {
		return nil, nil
	}
	return answer, nil
}

func (c *httpClient) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// rejected turns a partial success into an error.
func rejected(resp *collogspb.ExportLogsServiceResponse) error {
	if ps := resp.GetPartialSuccess(); ps.GetRejectedLogRecords() > 0 {
		return fmt.Errorf("otlp: the receiver rejected %d record(s): %s", ps.GetRejectedLogRecords(), ps.GetErrorMessage())
	}
	return nil
}
