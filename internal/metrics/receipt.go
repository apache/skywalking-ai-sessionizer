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

package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// receipt fixes one file's derivation before its request reaches the spool.
// A later continuation can change a retry's read-ahead. Recomputing after a
// crash would then mark extra calls counted beside a request that was already
// written without them. The exact request and its state changes travel together.
// Receipts live under their session, so removing a scenario removes them too.
type receipt struct {
	Schema  int                  `json:"schema"`
	Digest  string               `json:"digest"`
	Counted []string             `json:"counted,omitempty"`
	LastEnd map[string]time.Time `json:"last_end,omitempty"`
	Request []byte               `json:"request,omitempty"`
	Points  int                  `json:"points,omitempty"`
	Skipped bool                 `json:"skipped,omitempty"`
}

// commit applies the receipt to the session's state. It only adds counted
// calls and only moves a series forward, so receipts applied again, or in
// another order after an interrupted pass, give the same state.
func (r *receipt) commit(ss *sessionState) {
	for _, id := range r.Counted {
		ss.Calls[id] = true
	}
	for k, t := range r.LastEnd {
		if end := t.UnixNano(); end > ss.Series[k] {
			ss.Series[k] = end
		}
	}
}

// deriveReceipt reuses a committed decision, or publishes one after any grace.
// A crash before publication produces no spool request; one afterwards reuses
// both the exact bytes and the calls those bytes counted.
func (d *Deriver) deriveReceipt(path string, lf storage.LandedFile, following []storage.LandedFile, since time.Time, ss *sessionState, grace time.Duration) (*receipt, bool, error) {
	if r, err := loadReceipt(path, lf); err != nil || r != nil {
		return r, false, err
	}
	res, err := deriveFile(lf, following, since, ss)
	if err != nil {
		return nil, false, err
	}
	if res.openTail != "" && grace > 0 {
		fi, err := os.Stat(res.openTail)
		if err != nil {
			return nil, false, err
		}
		if d.Now().Sub(fi.ModTime()) < grace {
			return nil, true, nil
		}
	}
	r := &receipt{Schema: 1, Digest: res.digest, Counted: res.counted, LastEnd: res.lastEnd,
		Points: len(res.points), Skipped: !since.IsZero() && !res.latest.IsZero() && res.latest.Before(since)}
	if len(res.points) > 0 {
		r.Request, err = proto.Marshal(request(res.points, d.Version))
		if err != nil {
			return nil, false, err
		}
	}
	err = storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(r)
	})
	if errors.Is(err, storage.ErrExists) {
		// Another deriver settled this file first. Its decision is the one
		// that may already have reached the spool, so both writers must use it.
		r, err = loadReceipt(path, lf)
		if err == nil && r == nil {
			err = fmt.Errorf("metrics: receipt %s is still being published; the file is derived again on a later pass", path)
		}
	}
	return r, false, err
}

func loadReceipt(path string, lf storage.LandedFile) (*receipt, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// A receipt is never empty. An empty file is a reservation still being
	// published, or one a crash left; storage takes the second away once it
	// is old enough. Either way no decision was recorded.
	if len(data) == 0 {
		return nil, nil
	}
	var r receipt
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("metrics: read receipt %s: %w", path, err)
	}
	if r.Schema != 1 {
		return nil, fmt.Errorf("metrics: receipt %s has unsupported schema %d", path, r.Schema)
	}
	digest, err := storage.FileDigest(lf.Path)
	if err != nil {
		return nil, err
	}
	if r.Digest != digest {
		return nil, fmt.Errorf("metrics: receipt %s does not match its landed file", path)
	}
	return &r, nil
}
