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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// InboxDir is where an accepted request waits under a storage root until it
// has been converted into landed files.
//
// It exists because of the order the receiver has to work in: land, then
// answer, then convert. Answering first would lose a batch to a crash, and the
// client would never send it again — a rejected batch is retried, but an
// accepted one is gone. Converting first would hold the connection open for
// however long a session's lock is busy.
//
// A file here is write-once like any other, and is removed only once
// everything it carried has landed.
const InboxDir = "_langsmith"

// Inbox is the received-request inbox of one storage root.
type Inbox struct {
	dir string
	mu  sync.Mutex
}

// NewInbox opens the inbox of a root; nothing is created until Put.
func NewInbox(z *storage.Zone) *Inbox {
	return &Inbox{dir: filepath.Join(z.Root(), InboxDir)}
}

// Dir is the inbox directory.
func (i *Inbox) Dir() string { return i.dir }

// Received is the header a waiting request carries: enough to read the body
// again without the connection it arrived on.
type Received struct {
	V           int    `json:"v"`
	At          string `json:"at"`
	ContentType string `json:"content_type"`
	// Method and Path are how the request arrived. They are what tells a
	// single run object apart from an update to one: the same body means
	// a new run on POST and a change to an existing one on PATCH.
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
}

// Put writes one received request, header line then body, and returns its
// path. The body is the bytes that arrived, unchanged.
func (i *Inbox) Put(contentType, method, path string, body []byte, now time.Time) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := os.MkdirAll(i.dir, 0o755); err != nil {
		return "", err
	}
	seq, err := i.nextSeq()
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(Received{V: 1, At: now.UTC().Format(time.RFC3339Nano),
		ContentType: contentType, Method: method, Path: path, Bytes: len(body)})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("runs-%s-%06d.request",
		now.UTC().Format("20060102T150405.000000000Z"), seq)
	full := filepath.Join(i.dir, name)
	err = storage.WriteExclusive(full, storage.PermLanded, func(w io.Writer) error {
		if _, err := w.Write(append(header, '\n')); err != nil {
			return err
		}
		_, err := w.Write(body)
		return err
	})
	if err != nil {
		return "", err
	}
	return full, nil
}

// nextSeq keeps the inbox names unique and ordered.
//
// The filesystem is the authority, as it is for a session's sequence: a
// counter read off what is on disk cannot be reissued by a crash between
// writing a file and saving a number.
func (i *Inbox) nextSeq() (uint64, error) {
	items, err := os.ReadDir(i.dir)
	if err != nil {
		return 1, err
	}
	var highest uint64
	for _, it := range items {
		if seq, ok := inboxSeq(it.Name()); ok && seq > highest {
			highest = seq
		}
	}
	return highest + 1, nil
}

func inboxSeq(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "runs-") || !strings.HasSuffix(name, ".request") {
		return 0, false
	}
	trimmed := strings.TrimSuffix(name, ".request")
	at := strings.LastIndexByte(trimmed, '-')
	if at < 0 {
		return 0, false
	}
	seq, err := strconv.ParseUint(trimmed[at+1:], 10, 64)
	return seq, err == nil
}

// Waiting is one request still to be converted.
type Waiting struct {
	Path   string
	Seq    uint64
	Header Received
	Body   []byte
}

// List reads the header of every waiting request, oldest first.
//
// The bodies stay on disk. A request can be the whole 64 MB the receiver
// accepts, and an inbox that grew while nothing was collecting holds as many
// as arrived, so reading them all at once costs memory in proportion to the
// backlog. Read takes one when its turn comes and it is released after.
func (i *Inbox) List() ([]Waiting, error) {
	items, err := os.ReadDir(i.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Waiting
	for _, it := range items {
		seq, ok := inboxSeq(it.Name())
		if it.IsDir() || !ok {
			continue
		}
		path := filepath.Join(i.dir, it.Name())
		header, err := headerOf(path)
		if err != nil {
			return nil, err
		}
		out = append(out, Waiting{Path: path, Seq: seq, Header: header})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	return out, nil
}

// maxHeaderLine bounds the search for the header's newline, so a file that is
// not an inbox request at all cannot be read into memory whole while looking
// for one.
const maxHeaderLine = 64 << 10

// headerOf reads one waiting request's header line, and no more.
func headerOf(path string) (Received, error) {
	f, err := os.Open(path)
	if err != nil {
		return Received{}, err
	}
	defer f.Close()
	buf := make([]byte, maxHeaderLine)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Received{}, err
	}
	newline := indexByte(buf[:n], '\n')
	if newline < 0 {
		return Received{}, fmt.Errorf("langsmith: %s has no header line", path)
	}
	var header Received
	if err := json.Unmarshal(buf[:newline], &header); err != nil {
		return Received{}, fmt.Errorf("langsmith: %s: header: %w", path, err)
	}
	return header, nil
}

// Read fills in one waiting request's body.
func (i *Inbox) Read(w *Waiting) error {
	data, err := os.ReadFile(w.Path)
	if err != nil {
		return err
	}
	newline := indexByte(data, '\n')
	if newline < 0 {
		return fmt.Errorf("langsmith: %s has no header line", w.Path)
	}
	w.Body = data[newline+1:]
	return nil
}

// UnreadableDir holds the requests that could not be read into records.
//
// A body the collector cannot parse will not parse better next time, so
// leaving it where it was stopped every request behind it from ever being
// collected - one malformed body and the root went quiet. Moving it aside
// keeps the evidence, and says plainly why, while the rest goes on.
const UnreadableDir = "unreadable"

// Unreadable moves a request aside and records why.
func (i *Inbox) Unreadable(path, reason string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	dir := filepath.Join(i.dir, UnreadableDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	moved := filepath.Join(dir, filepath.Base(path))
	if err := os.Chmod(path, storage.PermState); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(path, moved); err != nil {
		return err
	}
	note := moved + ".reason"
	return storage.WriteAtomic(note, storage.PermState, func(w io.Writer) error {
		_, err := io.WriteString(w, reason+"\n")
		return err
	})
}

// Done removes a request whose records have all landed.
//
// A request is removed only after every session it touched has its files, so a
// crash in the middle converts the whole request again. That lands some records
// twice, which the index reads as duplicates and assembly skips: the reverse
// order would lose them, and nothing would ever say so.
func (i *Inbox) Done(path string) error {
	if err := os.Chmod(path, storage.PermState); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func indexByte(data []byte, b byte) int {
	for i, c := range data {
		if c == b {
			return i
		}
	}
	return -1
}
