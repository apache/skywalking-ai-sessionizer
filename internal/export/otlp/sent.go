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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EndpointOf names a receiver the way push.state records it.
//
// A gRPC receiver is grpc://host:port, or grpcs://host:port over TLS. An
// HTTP receiver is its base URL without trailing slashes, as the client keeps
// it. A gRPC endpoint is never a URL, so the two forms never meet. An empty
// endpoint names nothing.
func EndpointOf(protocol, endpoint string, tls bool) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	switch protocol {
	case ProtocolHTTP:
		return strings.TrimRight(endpoint, "/")
	case ProtocolGRPC, "":
		if tls {
			return "grpcs://" + endpoint
		}
		return "grpc://" + endpoint
	}
	return protocol + "://" + endpoint
}

// Sent is what push.state in a storage root records: the files sent, with
// the digest each had, the files a receiver rejected records of, and the
// receivers the files went to.
type Sent struct{ state *pushState }

// LoadSent reads push.state of a root. A root with none has sent nothing.
func LoadSent(root string) (*Sent, error) {
	s, err := loadState(filepath.Join(root, StateFile))
	if err != nil {
		return nil, err
	}
	return &Sent{state: s}, nil
}

// Digest is the digest rel had when it was sent, and whether it was sent.
// rel is the file's path under the root, with forward slashes.
func (s *Sent) Digest(rel string) (string, bool) {
	d, ok := s.state.files[rel]
	return d, ok
}

// Rejected reports whether a receiver took the request carrying rel and
// said it rejected some of the request's records. The protocol says not to
// send such a request again, so the file is recorded as sent all the same.
func (s *Sent) Rejected(rel string) bool { return s.state.rejected[rel] }

// Endpoints lists the receivers the recorded files were sent to, in order.
// It names "unknown" when push.state records files and no receiver.
func (s *Sent) Endpoints() []string { return sortedKeys(s.state.endpoints) }

// SentTo reports whether rel was sent to endpoint with the digest given, and
// no record of it was rejected.
//
// push.state records which receivers the files went to, not which file went
// to which. So SentTo holds only when endpoint is the one receiver
// push.state names. With two, which file reached which cannot be told, and
// with the receiver unknown nothing can be told. An empty endpoint is never
// sent to.
func (s *Sent) SentTo(endpoint, rel, digest string) bool {
	if endpoint == "" || endpoint == unknownEndpoint || len(s.state.endpoints) != 1 || !s.state.endpoints[endpoint] {
		return false
	}
	d, ok := s.state.files[rel]
	return ok && d == digest && !s.state.rejected[rel]
}

// ForgetGone drops the pushed and rejected lines of every path that owned
// reports true for and whose file is gone. Every endpoint line stays. The
// caller holds the root's export lock (storage.LockExport), because a pusher
// rewrites the same file.
//
// A line is dropped only once its file is gone, and a pusher sends only files
// that exist, so dropping a line sends nothing again. It would, if a file of
// the same name appeared later, and ruling that out is the caller's job. A
// root with no push.state is left without one.
func ForgetGone(root string, owned func(rel string) bool, now time.Time) error {
	if owned == nil {
		return nil
	}
	path := filepath.Join(root, StateFile)
	s, err := loadState(path)
	if err != nil {
		return err
	}
	changed := false
	for rel := range s.files {
		if owned(rel) && fileGone(root, rel) {
			delete(s.files, rel)
			changed = true
		}
	}
	for rel := range s.rejected {
		if owned(rel) && fileGone(root, rel) {
			delete(s.rejected, rel)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save(path, now)
}

// fileGone reports whether the file at rel under root surely does not exist.
// Any other answer, a permission error included, keeps its line.
func fileGone(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return errors.Is(err, fs.ErrNotExist)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
