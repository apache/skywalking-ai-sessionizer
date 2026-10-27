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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/server"
)

// testCA is a certificate authority a test makes, and signs with.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "asz test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a certificate for a server at 127.0.0.1, or for a client,
// and returns it and its key as PEM.
func (ca *testCA) issue(t *testing.T, client bool) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "asz"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	if client {
		tmpl.ExtKeyUsage, tmpl.IPAddresses = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// served serves a page that says ok behind cfg's protection, and returns
// its address.
func served(t *testing.T, cfg *config.Config) string {
	t.Helper()
	p, err := protectionOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := p.server(p.handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }), nil))
	go func() { _ = p.serve(hs, ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return ln.Addr().String()
}

// tlsClient trusts ca and may send a certificate.
func tlsClient(ca *testCA, cert *tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.pem)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	c := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		c.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: c, Protocols: &protocols}, Timeout: 10 * time.Second}
}

// An address with a certificate serves TLS, HTTP/2 included, and one with a
// client authority asks every client for a certificate it signed. Without a
// certificate the address serves HTTP/1.1, tokens and all.
func TestAnAddressServesTLSAsConfigured(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPEM, keyPEM := ca.issue(t, false)
	token, hash, err := server.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.RemoteServer.TokensFile = writeFile(t, dir, "tokens", []byte("prod "+hash+"\n"))
	cfg.RemoteServer.MaxConcurrentPerToken = 2
	cfg.RemoteServer.TLS = config.ServerTLS{CertFile: writeFile(t, dir, "cert.pem", certPEM), KeyFile: writeFile(t, dir, "key.pem", keyPEM)}
	do := func(c *http.Client, url string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return c.Do(req)
	}
	addr := served(t, cfg)
	resp, err := do(tlsClient(ca, nil), "https://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		t.Fatalf("over TLS: %d %s", resp.StatusCode, resp.Proto)
	}
	if resp, err := do(tlsClient(newCA(t), nil), "https://"+addr+"/"); err == nil {
		resp.Body.Close()
		t.Fatal("a client that trusts another authority connected")
	}

	clientCert, clientKey := ca.issue(t, true)
	cfg.RemoteServer.TLS.ClientCAFile = writeFile(t, dir, "clients.pem", ca.pem)
	addr = served(t, cfg)
	if resp, err := do(tlsClient(ca, nil), "https://"+addr+"/"); err == nil {
		resp.Body.Close()
		t.Fatal("a client without a certificate connected")
	}
	pair, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = do(tlsClient(ca, &pair), "https://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with a client certificate: %d", resp.StatusCode)
	}
	other := newCA(t)
	otherCert, otherKey := other.issue(t, true)
	otherPair, _ := tls.X509KeyPair(otherCert, otherKey)
	if resp, err := do(tlsClient(ca, &otherPair), "https://"+addr+"/"); err == nil {
		resp.Body.Close()
		t.Fatal("a certificate another authority signed was taken")
	}

	cfg.RemoteServer.TLS = config.ServerTLS{}
	addr = served(t, cfg)
	resp, err = do(&http.Client{Timeout: 10 * time.Second}, "http://"+addr+"/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Proto != "HTTP/1.1" {
		t.Fatalf("plain: %d %s", resp.StatusCode, resp.Proto)
	}
}

// TLS material that does not load stops the command before it serves.
func TestBadTLSMaterialStopsTheServer(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPEM, keyPEM := ca.issue(t, false)
	_, otherKey := ca.issue(t, false)
	cert, key := writeFile(t, dir, "cert.pem", certPEM), writeFile(t, dir, "key.pem", keyPEM)
	for name, tc := range map[string]config.ServerTLS{
		"a missing key file":       {CertFile: cert, KeyFile: filepath.Join(dir, "none")},
		"a key of another":         {CertFile: cert, KeyFile: writeFile(t, dir, "other.pem", otherKey)},
		"a certificate that is no": {CertFile: writeFile(t, dir, "junk.pem", []byte("junk")), KeyFile: key},
		"an empty client CA":       {CertFile: cert, KeyFile: key, ClientCAFile: writeFile(t, dir, "empty.pem", nil)},
	} {
		cfg := config.Default()
		cfg.RemoteServer.TLS = tc
		if _, err := protectionOf(cfg); err == nil {
			t.Errorf("%s was served", name)
		}
	}
}

// Tokens over plain HTTP on an address others can reach are said once at
// start; loopback and TLS say nothing.
func TestPlainTokensOffLoopbackAreWarned(t *testing.T) {
	dir := t.TempDir()
	_, hash, err := server.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	ca := newCA(t)
	certPEM, keyPEM := ca.issue(t, false)
	cfg := config.Default()
	cfg.RemoteServer.TokensFile = writeFile(t, dir, "tokens", []byte("prod "+hash+"\n"))
	for name, tc := range map[string]struct {
		addr string
		tls  bool
		warn bool
	}{
		"any address":     {"0.0.0.0:0", false, true},
		"loopback":        {"127.0.0.1:0", false, false},
		"any address TLS": {"0.0.0.0:0", true, false},
	} {
		cfg.RemoteServer.TLS = config.ServerTLS{}
		if tc.tls {
			cfg.RemoteServer.TLS = config.ServerTLS{CertFile: writeFile(t, dir, "c.pem", certPEM), KeyFile: writeFile(t, dir, "k.pem", keyPEM)}
		}
		p, err := protectionOf(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", tc.addr)
		if err != nil {
			t.Fatal(err)
		}
		said := stderrOf(t, func() { p.warn(ln) })
		ln.Close()
		if strings.Contains(said, "plain text") != tc.warn {
			t.Errorf("%s: %q", name, said)
		}
	}
}

// token create prints a token once with its line; with -file it adds the
// line, and a name already there prints no token at all.
func TestTokenCreate(t *testing.T) {
	var code int
	out := stdoutOf(t, func() { code = cmdToken([]string{"create", "prod"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "token "+server.TokenPrefix) || !strings.HasPrefix(lines[1], "tokens file line: prod ") {
		t.Fatalf("exit %d: %q", code, out)
	}
	file := filepath.Join(t.TempDir(), "tokens")
	out = stdoutOf(t, func() { code = cmdToken([]string{"create", "-file", file, "prod"}) })
	body, err := os.ReadFile(file)
	if err != nil || code != 0 || !strings.HasPrefix(string(body), "prod ") || !strings.Contains(out, "added to "+file) {
		t.Fatalf("exit %d, %q, file %q %v", code, out, body, err)
	}
	stderrOf(t, func() { out = stdoutOf(t, func() { code = cmdToken([]string{"create", "-file", file, "PROD"}) }) })
	if code == 0 || out != "" {
		t.Fatalf("a case alias: exit %d, printed %q", code, out)
	}
	if after, _ := os.ReadFile(file); string(after) != string(body) {
		t.Fatalf("a refused addition changed the file: %q", after)
	}
	for _, args := range [][]string{nil, {"create"}, {"remove", "prod"}, {"create", "a", "b"}} {
		stderrOf(t, func() { code = cmdToken(args) })
		if code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	stderrOf(t, func() { code = cmdToken([]string{"create", "a/b"}) })
	if code == 0 {
		t.Fatal("a bad name made a token")
	}
}
