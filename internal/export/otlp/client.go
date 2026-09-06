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
	return &grpcClient{conn: conn, logs: collogspb.NewLogsServiceClient(conn), headers: o.Headers, timeout: o.timeout()}, nil
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 60 * time.Second
	}
	return o.Timeout
}

func (c *grpcClient) Export(req *collogspb.ExportLogsServiceRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	for k, v := range c.headers {
		ctx = metadata.AppendToOutgoingContext(ctx, k, v)
	}
	resp, err := c.logs.Export(ctx, req)
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			return &Throttled{Detail: c.conn.Target() + ": " + err.Error()}
		}
		return fmt.Errorf("otlp: %s: %w", c.conn.Target(), err)
	}
	return rejected(resp)
}

func (c *grpcClient) Close() error { return c.conn.Close() }

// httpClient posts each request to the receiver's /v1/logs.
type httpClient struct {
	url     string
	headers map[string]string
	http    *http.Client
}

func newHTTP(o Options) (Client, error) {
	if !strings.HasPrefix(o.Endpoint, "http://") && !strings.HasPrefix(o.Endpoint, "https://") {
		return nil, fmt.Errorf("otlp: an http endpoint is a URL such as http://127.0.0.1:12800: %s", o.Endpoint)
	}
	return &httpClient{
		url:     strings.TrimRight(o.Endpoint, "/") + "/v1/logs",
		headers: o.Headers,
		http:    &http.Client{Timeout: o.timeout()},
	}, nil
}

func (c *httpClient) Export(req *collogspb.ExportLogsServiceRequest) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("otlp: %w", err)
	}
	hr, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	hr.Header.Set("User-Agent", "asz")
	for k, v := range c.headers {
		hr.Header.Set(k, v)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusTooManyRequests {
		t := &Throttled{Detail: c.url + " answered " + resp.Status}
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			t.After = time.Duration(secs) * time.Second
		}
		return t
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := answer
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		return fmt.Errorf("otlp: %s answered %s: %s", c.url, resp.Status, strings.TrimSpace(string(snippet)))
	}
	// A receiver answers in the request's encoding. Anything else, such as
	// an empty body from a proxy, is a full success as far as it says.
	var out collogspb.ExportLogsServiceResponse
	if len(answer) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-protobuf") {
		if err := proto.Unmarshal(answer, &out); err != nil {
			return fmt.Errorf("otlp: %s answered with a body that is not an OTLP response: %w", c.url, err)
		}
	}
	return rejected(&out)
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
