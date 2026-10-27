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
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/server"
)

// The limits every served address has.
const (
	headerWait = 10 * time.Second
	idleWait   = 90 * time.Second
	// credentialBytes bounds a certificate, key or CA file.
	credentialBytes = 1 << 20
)

// protection is what guards an address: its tokens, when remote_server
// names a tokens file, and its TLS, when it names a certificate.
type protection struct {
	guard *server.Guard
	tls   *tls.Config
}

// protectionOf reads what guards the address. A tokens file that cannot be
// read, or does not read as one, and TLS material that does not load stop
// the command before it serves anything.
func protectionOf(cfg *config.Config) (*protection, error) {
	p := &protection{}
	rs := cfg.RemoteServer
	if rs.Enabled() {
		keeper, err := server.LoadKeeper(rs.TokensFile, nil, func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "asz: "+format+"\n", args...)
		})
		if err != nil {
			return nil, err
		}
		p.guard = server.NewGuard(keeper, rs.MaxConcurrentPerToken, nil)
		p.guard.Log = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "asz: "+format+"\n", args...) }
	}
	if rs.TLSEnabled() {
		var err error
		if p.tls, err = serverTLS(rs.TLS); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// serverTLS loads the certificate an address serves with, and the
// authority its clients' certificates must come from when one is named.
func serverTLS(t config.ServerTLS) (*tls.Config, error) {
	cert, err := config.ReadSupplied(t.CertFile, credentialBytes)
	if err != nil {
		return nil, fmt.Errorf("remote_server.tls.cert_file: %w", err)
	}
	key, err := config.ReadSupplied(t.KeyFile, credentialBytes)
	if err != nil {
		return nil, fmt.Errorf("remote_server.tls.key_file: %w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("remote_server.tls: %w", err)
	}
	c := &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	if t.ClientCAFile != "" {
		ca, err := config.ReadSupplied(t.ClientCAFile, credentialBytes)
		if err != nil {
			return nil, fmt.Errorf("remote_server.tls.client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("remote_server.tls.client_ca_file holds no certificate")
		}
		c.ClientCAs, c.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return c, nil
}

// handler is the address's handler: the page alone without tokens, as it
// always was, and with tokens every route behind them, pull's included.
func (p *protection) handler(page, pull http.Handler) http.Handler {
	if p.guard == nil {
		return page
	}
	return p.guard.Protect(page, pull)
}

// server is the HTTP server for an address. Headers have ten seconds, idle
// connections ninety. With tokens on, OPTIONS * reaches the guard, which
// authenticates it like any request. Over TLS both ends may use HTTP/2.
func (p *protection) server(h http.Handler) *http.Server {
	hs := &http.Server{Handler: h, ReadHeaderTimeout: headerWait, IdleTimeout: idleWait,
		DisableGeneralOptionsHandler: p.guard != nil}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	if p.tls != nil {
		hs.TLSConfig = p.tls.Clone()
		protocols.SetHTTP2(true)
	}
	hs.Protocols = &protocols
	return hs
}

// serve serves hs on ln, over TLS when the address has it.
func (p *protection) serve(hs *http.Server, ln net.Listener) error {
	if p.tls != nil {
		return hs.ServeTLS(ln, "", "")
	}
	return hs.Serve(ln)
}

// scheme is how the address is reached.
func (p *protection) scheme() string {
	if p.tls != nil {
		return "https"
	}
	return "http"
}

// warn says, once at start, what a person should know of how the address is
// protected: tokens sent in plain text to an address others can reach can
// be read and used by anyone on the path.
func (p *protection) warn(ln net.Listener) {
	if p.guard == nil || p.tls != nil {
		return
	}
	if a, ok := ln.Addr().(*net.TCPAddr); ok && a.IP.IsLoopback() {
		return
	}
	fmt.Fprintln(os.Stderr, "asz: warning: tokens are on and TLS is off, on an address that is not loopback: "+
		"a token sent in plain text can be read and used by anyone on the path")
}

// pullServer is the pull server of a production root when tokens are on,
// or nil: without tokens no pull route is served, and a pulled snapshot's
// page serves none either.
func (p *protection) pullServer(cfg *config.Config, root string) (*server.Server, error) {
	if p.guard == nil || admitted != nil {
		return nil, nil
	}
	ttl, err := cfg.RemoteServer.SnapshotTTLDuration()
	if err != nil {
		return nil, err
	}
	every, err := cfg.RemoteServer.SweepInterval()
	if err != nil {
		return nil, err
	}
	return server.New(server.Options{Root: root, TTL: ttl, Every: every, Guard: p.guard,
		Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, "asz: "+format+"\n", args...) }}), nil
}

// serveWithPulls serves an address with the root's pull server beside the
// page until the command is interrupted or serving fails. It stops in one
// order: the address takes no new request and waits for running ones, then
// the pull server stops its workers and lets the root's pulls go.
func (p *protection) serveWithPulls(ln net.Listener, page http.Handler, pulls *server.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ran := make(chan error, 1)
	go func() { ran <- pulls.Run(ctx) }()
	hs := p.server(p.handler(page, pulls.Handler()))
	served := make(chan error, 1)
	go func() { served <- p.serve(hs, ln) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-served:
	case err = <-ran:
		_ = hs.Shutdown(context.Background())
		return err
	}
	stop()
	err = errors.Join(err, hs.Shutdown(context.Background()))
	err = errors.Join(err, <-ran)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
