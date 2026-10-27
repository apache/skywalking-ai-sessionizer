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

package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
)

// The limits of talking to a remote.
const (
	dialWait     = 10 * time.Second
	headerWait   = 60 * time.Second
	metadataWait = 2 * time.Minute
	stallWait    = 60 * time.Second
	idleWait     = 90 * time.Second
	pollEvery    = time.Second
	// failures is each budget's count: the fourth failure stops the remote.
	failures = 4
	// refreshLimit is how many manifests a conversation asks for after its
	// first.
	refreshLimit = 3
	// retryAfterLimit is the longest Retry-After a client waits. A longer
	// one stops that remote.
	retryAfterLimit = 60 * time.Second
	// perOrigin is how many requests one origin and token run at once.
	perOrigin = 2
)

// The ways a remote's answer stops work, by scope.
var (
	// ErrRemote stops every conversation of the remote: authentication,
	// trust, an invalid answer, an unsafe archive, an exhausted budget.
	ErrRemote = errors.New("pull: the remote cannot be read")
	// ErrConversation stops one conversation: a refusal that asking again
	// will not change.
	ErrConversation = errors.New("pull: the conversation cannot be pulled")
	// errGone is a snapshot the server no longer serves.
	errGone = errors.New("pull: the snapshot is gone")
)

// Remote is one configured asz server, reached for one command.
type Remote struct {
	Name  string
	base  *url.URL
	token string
	http  *http.Client
	slots chan struct{}
}

// RemoteOptions are what a remote is reached with.
type RemoteOptions struct {
	Name string
	Base *url.URL
	// Token is the token's text.
	Token string
	// CA, when set, replaces the system's roots. ClientCert is the client
	// certificate a server may ask for. ServerName is the name the server's
	// certificate is checked against when the URL's host is not it.
	CA         []byte
	ClientCert *tls.Certificate
	ServerName string
	// Slots are the request slots of one origin and token, shared by every
	// remote of the command that reaches that origin with that token.
	Slots chan struct{}
}

// NewRemote makes a remote's client. Each remote has a transport of its
// own, so its trust, certificate and server name never serve another: Go
// keys a pooled connection by its address, not by its TLS settings.
func NewRemote(o RemoteOptions) (*Remote, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: o.ServerName}
	if o.CA != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(o.CA) {
			return nil, fmt.Errorf("remote %s: its ca_file holds no certificate", o.Name)
		}
		tlsConfig.RootCAs = pool
	}
	if o.ClientCert != nil {
		tlsConfig.Certificates = []tls.Certificate{*o.ClientCert}
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	transport := &http.Transport{
		Proxy:                 proxyFor,
		DialContext:           (&net.Dialer{Timeout: dialWait}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   dialWait,
		ResponseHeaderTimeout: headerWait,
		IdleConnTimeout:       idleWait,
		MaxConnsPerHost:       perOrigin,
		MaxIdleConnsPerHost:   perOrigin,
		// Archive ranges are counted as they travel: nothing is decoded.
		DisableCompression: true,
		Protocols:          &protocols,
	}
	slots := o.Slots
	if slots == nil {
		slots = make(chan struct{}, perOrigin)
	}
	return &Remote{Name: o.Name, base: o.Base, token: o.Token, slots: slots,
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			// Go sends the token on to a redirect's host when it matches
			// or is a subdomain, so no redirect is followed.
			return http.ErrUseLastResponse
		}}}, nil
}

// proxyFor takes the environment's proxy for a request, and refuses any
// but an http:// one before anything is dialed: Go would use the origin's
// TLS settings on both hops of an https:// proxy.
func proxyFor(r *http.Request) (*url.URL, error) {
	u, err := http.ProxyFromEnvironment(r)
	if err != nil || u == nil {
		return u, err
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("%w: the proxy %s is %s://; use an http:// proxy or none", ErrRemote, u.Redacted(), u.Scheme)
	}
	return u, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// budget counts the failures one piece of work may meet.
type budget struct{ left int }

func newBudget() *budget { return &budget{left: failures} }

// charge spends one failure. The last one stops the remote.
func (b *budget) charge(err error) error {
	b.left--
	if b.left <= 0 {
		return fmt.Errorf("%w: %d failures, the last: %w", ErrRemote, failures, err)
	}
	return nil
}

// Tries is what one conversation's pull may still spend: the manifests it
// may ask for after its first, and the budget of an archive acquisition not
// yet complete, which a changed file list carries into the build made again.
type Tries struct {
	refreshes   int
	acquisition *budget
}

// take spends one of the conversation's refreshes.
func (t *Tries) take(why string) error {
	if t.refreshes >= refreshLimit {
		return fmt.Errorf("%w: %s, after %d fresh manifests", ErrConversation, why, refreshLimit)
	}
	t.refreshes++
	return nil
}

// backoff is the wait before the next try: half a second, one, then two,
// with jitter.
func backoff(n int) time.Duration {
	base := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}[min(n, 2)]
	return base/2 + time.Duration(rand.Int64N(int64(base)))
}

// answer is one metadata response, read by its caller within the request's
// two minutes. close lets its request slot go.
type answer struct {
	status int
	header http.Header
	body   io.ReadCloser
	done   func()
}

func (a *answer) close() {
	_ = a.body.Close()
	a.done()
}

// cutReader records a read of a body that failed, so a body that stopped
// arriving is told from one that arrived whole and does not read.
type cutReader struct {
	r   io.Reader
	err error
}

func (c *cutReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

// decode reads the answer's body with read, streamed. A body that stopped
// arriving is a failure asking again can fix; one that arrived whole and
// does not read stops the remote.
func (a *answer) decode(read func(io.Reader) error) error {
	c := &cutReader{r: a.body}
	err := read(c)
	if c.err != nil {
		return fmt.Errorf("the answer was cut: %w", c.err)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRemote, err)
	}
	return nil
}

// endpoint is a route under the remote's base path, each part escaped.
func (r *Remote) endpoint(parts ...string) string {
	u := *r.base
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = url.PathEscape(p)
	}
	u.RawPath = strings.TrimSuffix(r.base.EscapedPath(), "/") + "/pull/v1/" + strings.Join(escaped, "/")
	u.Path, _ = url.PathUnescape(u.RawPath)
	return u.String()
}

// take waits for one of the origin's request slots.
func (r *Remote) take(ctx context.Context) error {
	select {
	case r.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Remote) give() { <-r.slots }

// request is one request to the remote, with the token, asking for bytes
// as they are.
func (r *Remote) request(ctx context.Context, method, target string, body any, headers map[string]string) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept-Encoding", "identity")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// metadata sends one metadata request, which has two minutes to be sent
// and read whole, and holds one of the origin's slots until it is closed.
func (r *Remote) metadata(ctx context.Context, method, target string, body any, headers map[string]string) (*answer, error) {
	if err := r.take(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, metadataWait)
	done := func() {
		cancel()
		r.give()
	}
	req, err := r.request(ctx, method, target, body, headers)
	if err != nil {
		done()
		return nil, err
	}
	resp, err := r.http.Do(req) //nolint:bodyclose // the answer's close closes it, once its caller has read it
	if err != nil {
		done()
		return nil, transportError(err)
	}
	return &answer{status: resp.StatusCode, header: resp.Header, body: resp.Body, done: done}, nil
}

// transportError says whether a failed request can be tried again. A
// certificate that does not verify, a TLS refusal from the server, such as
// of a missing client certificate, or a refused proxy will fail again.
func transportError(err error) error {
	var verify *tls.CertificateVerificationError
	var alert tls.AlertError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var op *net.OpError
	switch {
	case errors.Is(err, ErrRemote):
		return err
	case errors.As(err, &verify), errors.As(err, &alert), errors.As(err, &unknown), errors.As(err, &host):
		return fmt.Errorf("%w: %w", ErrRemote, err)
	case errors.As(err, &op) && op.Op == "remote error":
		// Over TCP, Go reports an alert the server sent, such as one for a
		// missing client certificate, as a net.OpError of this name.
		return fmt.Errorf("%w: %w", ErrRemote, err)
	}
	return err
}

// retryable says whether a status is tried again.
func retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// again waits before the next try after a failure and charges it: the
// answer's Retry-After when it has one, up to a minute, else the backoff.
// header is nil when no answer came.
func again(ctx context.Context, b *budget, n int, header http.Header, err error) error {
	// A snapshot that is gone is not tried again: its manifest is.
	if errors.Is(err, ErrRemote) || errors.Is(err, ErrConversation) || errors.Is(err, errGone) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cerr := b.charge(err); cerr != nil {
		return cerr
	}
	d := backoff(n)
	if v := header.Get("Retry-After"); v != "" {
		var after time.Duration
		if secs, perr := strconv.ParseUint(v, 10, 32); perr == nil {
			after = time.Duration(secs) * time.Second
		} else if t, perr := http.ParseTime(v); perr == nil {
			after = max(time.Until(t), 0)
		} else {
			after = -1
		}
		if after > retryAfterLimit {
			return fmt.Errorf("%w: it asks to be tried again in %s", ErrRemote, after.Round(time.Second))
		}
		if after >= 0 {
			d = after
		}
	}
	return sleep(ctx, d)
}

// problemOf reads an answer's problem. One that does not read stops the
// remote.
func problemOf(a *answer) (format.Problem, error) {
	if a.header.Get("Content-Type") != "application/problem+json" {
		return format.Problem{}, fmt.Errorf("%w: a %d without a problem", ErrRemote, a.status)
	}
	var p format.Problem
	err := a.decode(func(r io.Reader) error {
		var err error
		p, err = format.ReadProblem(r, a.status)
		return err
	})
	return p, err
}

// refusal turns an answer that is not tried again into the error of its
// scope.
func refusal(a *answer, what string) error {
	p, err := problemOf(a)
	if err != nil {
		return err
	}
	switch a.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s: %d %s", ErrRemote, what, a.status, p.Detail)
	case http.StatusGone:
		return fmt.Errorf("%w: %s", errGone, p.Detail)
	}
	return fmt.Errorf("%w: %s: %d %s", ErrConversation, what, a.status, p.Detail)
}

// failed reads a retryable answer's problem when it has one, for the
// typed failures that act by name. A retryable answer without one, as a
// proxy's, is only a status.
func failed(a *answer) (format.Problem, error) {
	if a.header.Get("Content-Type") != "application/problem+json" {
		return format.Problem{Status: a.status}, nil
	}
	return problemOf(a)
}

// List asks for one page of the remote's conversations.
func (r *Remote) List(ctx context.Context, query url.Values) (*format.Listing, error) {
	target := r.endpoint("conversations")
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	b := newBudget()
	for n := 0; ; n++ {
		var header http.Header
		a, err := r.metadata(ctx, http.MethodGet, target, nil, nil)
		if err == nil {
			header = a.header
			var l *format.Listing
			switch {
			case a.status == http.StatusOK:
				err = a.decode(func(body io.Reader) error {
					var rerr error
					l, rerr = format.ReadListing(body)
					return rerr
				})
			case !retryable(a.status):
				err = refusal(a, "the listing")
				if errors.Is(err, errGone) {
					err = fmt.Errorf("%w: the listing answered 410", ErrRemote)
				}
			default:
				err = retryError(a, b, "the listing")
			}
			a.close()
			if err == nil {
				return l, nil
			}
		}
		if err := again(ctx, b, n, header, err); err != nil {
			return nil, err
		}
	}
}

// retryError reads a retryable answer, its problem checked when it has one:
// storage-state stops the work it answers, charged first, and anything else
// is a failure asking again can fix.
func retryError(a *answer, b *budget, what string) error {
	p, err := failed(a)
	if err != nil {
		return err
	}
	if p.Type == format.ProblemStorageState {
		_ = b.charge(errors.New(p.Detail))
		return fmt.Errorf("%w: %s: %s", ErrConversation, what, p.Detail)
	}
	return fmt.Errorf("%s answered %d %s", what, a.status, p.Detail)
}

// Manifest freezes a conversation's head on the remote and returns its
// manifest, with the snapshot's tag and a lease on it, under a budget of
// its own. A refresh it needs, its freeze gone, starts a fresh budget.
func (r *Remote) Manifest(ctx context.Context, conversation string, tries *Tries) (*format.Manifest, format.Response, error) {
	return r.manifest(ctx, conversation, nil, tries)
}

// What a manifest answer asks of the client next.
type manifestNext int

const (
	manifestRetry manifestNext = iota
	manifestPoll
	manifestFreeze
	manifestRefresh
)

// manifest is Manifest under acquisition's budget, when the snapshot went
// away while an archive was fetched, or under its own when acquisition is
// nil. The freeze is polled once a second, and polls charge nothing. A typed
// freeze failure is charged once per freeze and attempt; it, and a saved
// source failure a poll answers, drop the freeze, and a new one is asked
// for within the same budget.
func (r *Remote) manifest(ctx context.Context, conversation string, acquisition *budget, tries *Tries) (*format.Manifest, format.Response, error) {
	target := r.endpoint("conversations", conversation, "manifest")
	b := acquisition
	if b == nil {
		b = newBudget()
	}
	freeze := ""
	charged := map[string]bool{}
	for n := 0; ; {
		var headers map[string]string
		if freeze != "" {
			headers = map[string]string{"Asz-Pull-Freeze": freeze}
		}
		var header http.Header
		a, err := r.metadata(ctx, http.MethodGet, target, nil, headers)
		if err == nil {
			header = a.header
			var m *format.Manifest
			var resp format.Response
			var next manifestNext
			m, resp, next, err = r.manifestAnswer(a, conversation, &freeze, charged, b, tries)
			a.close()
			switch {
			case err == nil && m != nil:
				return m, resp, nil
			case err == nil && next == manifestPoll:
				if err := sleep(ctx, pollEvery); err != nil {
					return nil, format.Response{}, err
				}
				continue
			case err == nil && next == manifestRefresh && acquisition == nil:
				// A refresh outside an archive acquisition starts a fresh
				// budget; one inside it spends what the acquisition has.
				b = newBudget()
				fallthrough
			case err == nil:
				// A new freeze, asked for after a backoff.
				if err := sleep(ctx, backoff(n)); err != nil {
					return nil, format.Response{}, err
				}
				continue
			}
		}
		if err := again(ctx, b, n, header, err); err != nil {
			return nil, format.Response{}, err
		}
		n++
	}
}

// manifestAnswer reads one answer of the manifest route: the manifest, a
// poll to come, a new freeze to ask for, a refresh, or an error.
func (r *Remote) manifestAnswer(a *answer, conversation string, freeze *string, charged map[string]bool, b *budget, tries *Tries) (*format.Manifest, format.Response, manifestNext, error) {
	switch a.status {
	case http.StatusOK:
		var m *format.Manifest
		var resp format.Response
		err := a.decode(func(body io.Reader) error {
			var rerr error
			m, resp, rerr = format.ReadManifestResponse(body, conversation)
			return rerr
		})
		return m, resp, manifestRetry, err
	case http.StatusAccepted:
		var p format.ManifestPending
		err := a.decode(func(body io.Reader) error {
			var rerr error
			p, rerr = format.ReadManifestPending(body, conversation)
			return rerr
		})
		if err == nil && *freeze != "" && p.Freeze != *freeze {
			err = fmt.Errorf("%w: a poll of freeze %s answered freeze %s", ErrRemote, *freeze, p.Freeze)
		}
		*freeze = p.Freeze
		return nil, format.Response{}, manifestPoll, err
	case http.StatusConflict:
		// The freeze is gone, or its result expired, or a removal runs: a
		// new freeze, as a refresh.
		if _, err := problemOf(a); err != nil {
			return nil, format.Response{}, manifestRetry, err
		}
		*freeze = ""
		return nil, format.Response{}, manifestRefresh, tries.take("its freeze went away")
	case http.StatusGone:
		if _, err := problemOf(a); err != nil {
			return nil, format.Response{}, manifestRetry, err
		}
		return nil, format.Response{}, manifestRetry, fmt.Errorf("%w: the manifest answered 410", ErrConversation)
	}
	if !retryable(a.status) {
		return nil, format.Response{}, manifestRetry, refusal(a, "the manifest")
	}
	p, err := failed(a)
	if err != nil {
		return nil, format.Response{}, manifestRetry, err
	}
	switch p.Type {
	case format.ProblemStorageState:
		_ = b.charge(errors.New(p.Detail))
		return nil, format.Response{}, manifestRetry, fmt.Errorf("%w: %s", ErrConversation, p.Detail)
	case format.ProblemSourceRead, format.ProblemSourceChange:
		// A poll answering its freeze's saved failure: the freeze is done
		// for, and a new one is asked for after the wait this charges.
		*freeze = ""
	case format.ProblemFreezeFailed:
		if a.status != http.StatusInternalServerError || p.Conversation != conversation || *freeze == "" || p.Freeze != *freeze || p.Attempt < 3 {
			return nil, format.Response{}, manifestRetry, fmt.Errorf("%w: a freeze failure of another freeze", ErrRemote)
		}
		key := p.Freeze + "/" + strconv.FormatUint(p.Attempt, 10)
		*freeze = ""
		if charged[key] {
			return nil, format.Response{}, manifestFreeze, nil
		}
		charged[key] = true
		if err := b.charge(errors.New(p.Detail)); err != nil {
			return nil, format.Response{}, manifestRetry, err
		}
		return nil, format.Response{}, manifestFreeze, nil
	}
	return nil, format.Response{}, manifestRetry, fmt.Errorf("the manifest answered %d %s", a.status, p.Detail)
}

// Done releases a lease. A snapshot already gone counts as released.
func (r *Remote) Done(ctx context.Context, conversation string, resp format.Response) error {
	target := r.endpoint("conversations", conversation, "snapshots", resp.Snapshot, "done")
	b := newBudget()
	for n := 0; ; n++ {
		var header http.Header
		a, err := r.metadata(ctx, http.MethodPost, target, map[string]string{"lease": resp.Lease}, nil)
		if err == nil {
			header = a.header
			gone := false
			switch {
			case a.status == http.StatusNoContent:
			case a.status == http.StatusGone:
				_, err = problemOf(a)
				gone = err == nil
			case !retryable(a.status):
				err = refusal(a, "the release")
			default:
				err = retryError(a, b, "the release")
			}
			a.close()
			if a.status == http.StatusNoContent || gone {
				return nil
			}
		}
		if err := again(ctx, b, n, header, err); err != nil {
			return err
		}
	}
}
