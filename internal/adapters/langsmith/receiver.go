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

package langsmith

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Stats counts what the receiver did since it started.
type Stats struct {
	Requests    int64 // requests accepted and put in the inbox
	Operations  int64 // run arrivals those requests carried
	Bytes       int64 // bytes accepted
	Info        int64 // capability probes answered
	Feedback    int64 // feedback parts accepted and not landed
	Attachments int64 // attachment parts accepted and not landed
	Refused     int64 // requests refused, with a reason the client can read
	Errors      int64
}

// Receiver takes what the LangSmith tracing client sends.
//
// It answers only what that client asks for. Everything it accepts goes to the
// inbox first and is converted afterwards, so a crash between accepting and
// converting costs a repeat and never a batch.
type Receiver struct {
	Zone *storage.Zone
	// Listen is the address to serve on, such as 127.0.0.1:1985. It binds
	// locally by default because what arrives here is whole prompts and whole
	// tool output.
	Listen string
	// Token, when set, is required in the x-api-key header. Empty accepts any
	// key, which is what a local collector wants: the client insists on
	// sending one and has nothing to prove.
	Token string
	Now   func() time.Time

	inbox       *Inbox
	requests    atomic.Int64
	operations  atomic.Int64
	bytes       atomic.Int64
	info        atomic.Int64
	feedback    atomic.Int64
	attachments atomic.Int64
	refused     atomic.Int64
	errs        atomic.Int64

	mu   sync.Mutex
	ln   net.Listener
	web  *http.Server
	done chan struct{}
}

// Stats reports the counts so far.
func (r *Receiver) Stats() Stats {
	return Stats{Requests: r.requests.Load(), Operations: r.operations.Load(),
		Bytes: r.bytes.Load(), Info: r.info.Load(), Feedback: r.feedback.Load(),
		Attachments: r.attachments.Load(), Refused: r.refused.Load(),
		Errors: r.errs.Load()}
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

// Start opens the listener and serves until Stop.
func (r *Receiver) Start() error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Listen == "" {
		return errors.New("langsmith-ingest: no listen address")
	}
	r.inbox = NewInbox(r.Zone)
	ln, err := net.Listen("tcp", r.Listen)
	if err != nil {
		return fmt.Errorf("langsmith-ingest: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", r.serveInfo)
	mux.HandleFunc("/runs/multipart", r.serveRuns)
	mux.HandleFunc("/runs/batch", r.serveRuns)
	mux.HandleFunc("/runs", r.serveRuns)
	mux.HandleFunc("/runs/", r.serveRuns)
	mux.HandleFunc("/", r.serveUnknown)
	r.web = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	r.mu.Lock()
	r.ln, r.done = ln, make(chan struct{})
	r.mu.Unlock()
	go func() {
		defer close(r.done)
		_ = r.web.Serve(ln)
	}()
	return nil
}

// Stop closes the listener and waits for the server to finish.
func (r *Receiver) Stop() {
	r.mu.Lock()
	ln, done := r.ln, r.done
	r.mu.Unlock()
	if ln == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.web.Shutdown(ctx)
	<-done
}

// serveInfo answers the capability probe the client sends before every send.
//
// What is left out matters more than what is in. The client compresses only
// when instance_flags says zstd is enabled, so not advertising it is what
// keeps the body plain and keeps a compression library out of this project
// altogether. A receiver that never answered would still work — the client
// logs a warning and carries on — but it would batch by its own defaults and
// the warning would look like a fault.
func (r *Receiver) serveInfo(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.info.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":                 "asz/" + Version,
		"license_expiration_time": nil,
		"batch_ingest_config": map[string]any{
			"use_multipart_endpoint":    true,
			"size_limit":                BatchSizeLimit,
			"size_limit_bytes":          BatchSizeLimitBytes,
			"scale_up_qsize_trigger":    1000,
			"scale_up_nthreads_limit":   16,
			"scale_down_nempty_trigger": 4,
		},
	})
}

// serveRuns accepts a batch of runs.
func (r *Receiver) serveRuns(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !r.authorized(req) {
		r.refused.Add(1)
		writeJSON(w, http.StatusForbidden, map[string]any{
			"detail": "this receiver was configured with a token; send it as x-api-key"})
		return
	}
	body, err := r.read(req)
	if err != nil {
		r.refused.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	contentType := req.Header.Get("Content-Type")
	// Reading the request here is the only way to refuse one the collector
	// could not convert. A body accepted and never convertible would be a 202
	// that meant nothing, visible only in a log.
	counted, err := countOperations(body, contentType)
	if err != nil {
		r.refused.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	if _, err := r.inbox.Put(contentType, req.Method, req.URL.Path, body, r.Now()); err != nil {
		r.errs.Add(1)
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"detail": "the receiver could not keep this request"})
		return
	}
	r.requests.Add(1)
	r.bytes.Add(int64(len(body)))
	r.operations.Add(int64(counted.Operations))
	r.feedback.Add(int64(counted.Feedback))
	r.attachments.Add(int64(counted.Attachments))
	// The client takes any success. 202 is what the documented endpoint
	// answers and it is the honest one: this request is kept, not yet read
	// into a conversation.
	writeJSON(w, http.StatusAccepted, map[string]any{"message": "accepted"})
}

// serveUnknown answers a path this receiver does not serve.
//
// It says so rather than returning a bare 404, because the one thing a person
// pointing a client here gets wrong is the endpoint, and a 404 with no words
// looks like the receiver is down.
func (r *Receiver) serveUnknown(w http.ResponseWriter, req *http.Request) {
	r.refused.Add(1)
	writeJSON(w, http.StatusNotFound, map[string]any{
		"detail": "this is an asz langsmith-ingest receiver; it serves /info and /runs*, and " +
			req.URL.Path + " is not one of them"})
}

// read takes the body, refusing one too large to be a batch and decoding the
// encodings this receiver told the client it would take.
func (r *Receiver) read(req *http.Request) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(nil, req.Body, MaxRequest)
	switch strings.ToLower(req.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("the body is not gzip: %w", err)
		}
		defer gz.Close()
		// The bound above limits what arrives, not what it becomes. A few
		// hundred kilobytes of gzip expands to gigabytes, so the limit has to
		// be applied again after decoding or it is not a limit at all. One
		// byte over is read on purpose, so that a body exactly at the bound
		// is still accepted and the one past it is known to be too large.
		reader = io.LimitReader(gz, MaxRequest+1)
	case "zstd":
		// Never advertised, so a client sending it has ignored what /info
		// said. Naming the flag is what lets whoever runs it fix the cause
		// rather than guess at it.
		return nil, errors.New(
			"this receiver does not advertise instance_flags.zstd_compression_enabled, " +
				"so it cannot read a zstd body")
	default:
		return nil, fmt.Errorf("content encoding %q is not one this receiver reads",
			req.Header.Get("Content-Encoding"))
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("the body could not be read: %w", err)
	}
	if len(body) > MaxRequest {
		return nil, fmt.Errorf("the body is larger than this receiver accepts (%d bytes)", MaxRequest)
	}
	return body, nil
}

func (r *Receiver) authorized(req *http.Request) bool {
	if r.Token == "" {
		return true
	}
	given := req.Header.Get("x-api-key")
	return subtle.ConstantTimeCompare([]byte(given), []byte(r.Token)) == 1
}

// countOperations reads the body far enough to know it can be converted.
type counted struct{ Operations, Feedback, Attachments int }

func countOperations(body []byte, contentType string) (counted, error) {
	if !strings.Contains(contentType, "multipart/") {
		// The JSON endpoints carry {"post": [...], "patch": [...]}. They are
		// accepted so an older client is not turned away, and the collector
		// reads them the same way.
		var batch struct {
			Post  []json.RawMessage `json:"post"`
			Patch []json.RawMessage `json:"patch"`
		}
		if err := json.Unmarshal(body, &batch); err == nil && (len(batch.Post) > 0 || len(batch.Patch) > 0) {
			// Each entry has to be a run. A batch of {"post":[1]} counted as
			// one operation and was answered 202, and the collector then
			// failed on it every pass for ever.
			for _, group := range [][]json.RawMessage{batch.Post, batch.Patch} {
				for _, raw := range group {
					if err := checkRun(raw); err != nil {
						return counted{}, err
					}
				}
			}
			return counted{Operations: len(batch.Post) + len(batch.Patch)}, nil
		}
		// A single run, which is what the documented POST /runs and
		// PATCH /runs/{id} take. Accepting any JSON at all was worse than
		// refusing: a body with no run in it was answered 202 and then
		// silently dropped by the collector, so the client believed it had
		// been kept.
		var single struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &single) != nil || single.ID == "" {
			return counted{}, errors.New(
				"the body is neither multipart, nor a {post, patch} batch, nor a single run with an id")
		}
		return counted{Operations: 1}, nil
	}
	parsed, err := ParseMultipart(strings.NewReader(string(body)), contentType)
	if err != nil {
		return counted{}, err
	}
	// A part named for a run still has to carry one. Nothing looked inside
	// before, so a body of well-formed parts holding nothing readable was
	// accepted and then failed in the collector instead.
	for _, op := range parsed.Operations {
		if err := checkRun(op.Envelope); err != nil {
			return counted{}, fmt.Errorf("%s.%s: %w", op.Op, op.RunID, err)
		}
		// And every field beside it. A well-formed envelope with a broken
		// outputs part passed, and the break was only found when the writer
		// refused the record - by which time the request had been accepted,
		// and it stopped every pass from then on.
		for name, raw := range op.Fields {
			if !json.Valid(raw) {
				return counted{}, fmt.Errorf("%s.%s.%s is not valid JSON", op.Op, op.RunID, name)
			}
		}
	}
	out := counted{Operations: len(parsed.Operations), Feedback: parsed.Feedback,
		Attachments: parsed.Attachments}
	if out.Operations == 0 && out.Feedback == 0 && out.Attachments == 0 {
		// A body declared multipart that holds nothing this receiver
		// recognises reads as an empty one, and answering it 202 says it was
		// kept. Nothing was, so it says so instead.
		return counted{}, errors.New("this multipart body carries no run, no feedback and no attachment")
	}
	return out, nil
}

// checkRun reads one envelope far enough to know the collector will be able
// to. Refusing here is what makes the 202 mean something: the client retries
// a refusal and never retries an acceptance.
func checkRun(raw json.RawMessage) error {
	// The whole envelope, as the collector will read it. Checking only the
	// id let a run through whose other fields were the wrong type, and the
	// collector then failed on it every pass.
	var envelope Run
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("a run is not one this receiver can read: %w", err)
	}
	if envelope.ID == "" {
		return errors.New("a run carries no id")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
