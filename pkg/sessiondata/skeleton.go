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

package sessiondata

import (
	"compress/gzip"
	"io"
	"os"
	"strings"
)

// SkeletonExt is the extension of a skeleton: the file that takes a landed
// file's place when retention removes it. A skeleton is Session Data,
// compressed with gzip, that keeps the header, every record and every part in
// its place, and only the fields a conversation's structure is built from. It
// is local and never sent.
const SkeletonExt = ".sk"

// Removed is what a skeleton's header says about the landed file it took the
// place of. Only a skeleton's header carries it.
type Removed struct {
	// Digest, Size and Records describe the landed file: the SHA-256 of its
	// whole bytes, which a round's input digest binds, its size in bytes, and
	// its number of records.
	Digest  string `json:"digest"`
	Size    int64  `json:"size"`
	Records int    `json:"records"`
	// At is when the file was removed. Generation is which removal of its
	// session removed it, counting from 1.
	At         string `json:"at"`
	Generation int    `json:"generation"`
	// Receiver says what the receiver confirmed of the file before it was
	// removed, and Owner is the owner its session is sent under.
	Receiver string `json:"receiver"`
	Owner    string `json:"owner,omitempty"`
}

// What a receiver confirmed of a landed file before it was removed.
const (
	// ReceiverAccepted is a file the receiver took whole.
	ReceiverAccepted = "accepted"
	// ReceiverRejected is a file the receiver took while it rejected some of
	// its records.
	ReceiverRejected = "rejected"
	// ReceiverUnconfirmed is a file no answer confirmed: it was never sent, or
	// its request had no answer.
	ReceiverUnconfirmed = "unconfirmed"
)

// IsSkeleton reports whether path names a skeleton.
func IsSkeleton(path string) bool { return strings.HasSuffix(path, SkeletonExt) }

// Open opens a landed file or a skeleton for reading, so a reader reads both
// the same way: a skeleton is read through its compression.
func Open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !IsSkeleton(path) {
		return f, nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &skeletonFile{zr, f}, nil
}

type skeletonFile struct {
	*gzip.Reader
	f *os.File
}

func (s *skeletonFile) Close() error {
	err := s.Reader.Close()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	return err
}
