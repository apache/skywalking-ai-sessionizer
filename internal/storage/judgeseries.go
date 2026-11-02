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

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// JudgeDir holds what asz server's judge policy keeps in a production root:
// the moment it was first turned on, a receipt for every automatic attempt,
// and the lock one automatic run holds. Its owner is the policy's
// coordinator, and every write in it happens under the root lock. A manual
// asz judge writes nothing there.
const JudgeDir = "_judge"

// The kinds of receipt.
const (
	ReceiptRun  = "run"
	ReceiptSkip = "skip"
)

// Receipt is one automatic attempt, written and synced before its run's
// directory is reserved, in the same hold of the root lock. The series of a
// conversation is its receipts: which talks each covered, at which view.
type Receipt struct {
	// Number orders the conversation's receipts: the next number, taken
	// under the root lock, whatever the clock did.
	Number       int    `json:"number"`
	Kind         string `json:"kind"`
	Conversation string `json:"conversation"`
	Session      string `json:"session"`
	Time         string `json:"time"`
	// Round and Digest are the head the attempt was decided at.
	Round  uint64 `json:"round"`
	Digest string `json:"digest"`
	// Landed is the session's highest landed sequence then: an attempt
	// that did not succeed is not tried again until a higher one lands.
	Landed uint64 `json:"landed"`
	// Parser and Policy are the chain's interpretation, under which the
	// views were taken.
	Parser string `json:"parser"`
	Policy string `json:"policy"`
	// Talks are the main talks the attempt selected, each with its view
	// digest.
	Talks []ReceiptTalk `json:"talks"`
	// Target is the window's end, the last record time the run is shown,
	// in unix milliseconds.
	Target int64 `json:"target"`
	Steps  int   `json:"steps"`
	// Run is the run number the attempt reserves, for a run.
	Run int `json:"run,omitempty"`
}

// ReceiptTalk is one main talk an attempt selected, and its view digest.
type ReceiptTalk struct {
	ID   string `json:"id"`
	View string `json:"view"`
}

var receiptRe = regexp.MustCompile(`^(\d{6,})\.json$`)

func receiptsDir(z *Zone, conversation string) string {
	return filepath.Join(z.Root(), JudgeDir, "runs", conversation)
}

// Receipts reads a conversation's receipts, in number order. One that does
// not read is an error, never passed over: the series would be wrong.
func Receipts(z *Zone, conversation string) ([]Receipt, error) {
	items, err := os.ReadDir(receiptsDir(z, conversation))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Receipt
	for _, it := range items {
		if !receiptRe.MatchString(it.Name()) {
			continue
		}
		path := filepath.Join(receiptsDir(z, conversation), it.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var r Receipt
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("storage: receipt %s: %w", path, err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// WriteReceipt gives a receipt the conversation's next number and writes
// it, read-only and synced with its directory. The caller holds the root
// lock.
func WriteReceipt(z *Zone, r *Receipt) error {
	have, err := Receipts(z, r.Conversation)
	if err != nil {
		return err
	}
	r.Number = 1
	if len(have) > 0 {
		r.Number = have[len(have)-1].Number + 1
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := receiptsDir(z, r.Conversation)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return WriteAtomicDurable(filepath.Join(dir, fmt.Sprintf("%06d.json", r.Number)), 0o444, func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	}, nil)
}

// AllReceipts reads the receipts of every conversation of the root. A
// receipt that does not read is an error.
func AllReceipts(z *Zone) ([]Receipt, error) {
	convs, err := os.ReadDir(filepath.Join(z.Root(), JudgeDir, "runs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Receipt
	for _, c := range convs {
		if !c.IsDir() {
			continue
		}
		rs, err := Receipts(z, c.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// RunReceiptsSince counts the run receipts of every conversation written at
// or after a time, whether their runs exist or not: what the daily allowance
// counts.
func RunReceiptsSince(z *Zone, since time.Time) (int, error) {
	all, err := AllReceipts(z)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range all {
		at, err := time.Parse(time.RFC3339Nano, r.Time)
		if err != nil {
			return 0, fmt.Errorf("storage: receipt %d of %s has no time: %w", r.Number, r.Conversation, err)
		}
		if r.Kind == ReceiptRun && !at.Before(since) {
			n++
		}
	}
	return n, nil
}

func sincePath(z *Zone) string { return filepath.Join(z.Root(), JudgeDir, "since") }

// LoadSince reads the moment the policy was first turned on; false when it
// was never written.
func LoadSince(z *Zone) (time.Time, bool, error) {
	data, err := os.ReadFile(sincePath(z))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("storage: %s: %w", sincePath(z), err)
	}
	return t, true, nil
}

// SaveSince writes the moment the policy was first turned on, once. The
// caller holds the root lock and found none.
func SaveSince(z *Zone, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(sincePath(z)), 0o755); err != nil {
		return err
	}
	return WriteAtomicDurable(sincePath(z), 0o444, func(w io.Writer) error {
		_, err := fmt.Fprintln(w, t.UTC().Format(time.RFC3339Nano))
		return err
	}, nil)
}

// TryLockAutomatic takes the lock one automatic run holds in a root, without
// waiting: held by another server on the root, it is ErrBusy. The caller
// holds the root lock.
func TryLockAutomatic(z *Zone) (*SessionLock, error) {
	dir := filepath.Join(z.Root(), JudgeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return TryLockFile(filepath.Join(dir, "automatic.lock"))
}

// ProbeAutomatic says whether an automatic run holds the root's lock, without
// waiting and without making anything.
func ProbeAutomatic(z *Zone) (bool, error) {
	return ProbeFileLock(filepath.Join(z.Root(), JudgeDir, "automatic.lock"))
}

var stampRe = regexp.MustCompile(`-(\d{8}T\d{6}\.\d{9}Z)-\d{6,}\.s[dk]$`)

// Collected is when the newest landed file of a session was collected: the
// stamp in its name, the start of the pass that landed it, with the file's
// sequence. The stamp is in the name, so it survives a restart and a copy.
func Collected(z *Zone, session string) (time.Time, uint64, error) {
	files, err := LandedFiles(z, session)
	if err != nil {
		return time.Time{}, 0, err
	}
	var newest *LandedFile
	for i := range files {
		if newest == nil || files[i].Seq > newest.Seq {
			newest = &files[i]
		}
	}
	if newest == nil {
		return time.Time{}, 0, fmt.Errorf("storage: session %s has no landed file", session)
	}
	m := stampRe.FindStringSubmatch(filepath.Base(newest.Path))
	if m == nil {
		return time.Time{}, 0, fmt.Errorf("storage: %s carries no collection stamp", newest.Path)
	}
	t, err := time.Parse("20060102T150405.000000000Z", m[1])
	if err != nil {
		return time.Time{}, 0, err
	}
	return t, newest.Seq, nil
}

// ReceiptRunMatches says whether a run's admission names the receipt that
// reserved it: only then may the receipt read the run's close as its own.
func ReceiptRunMatches(runDir string, r Receipt) (bool, error) {
	a, err := ReadRunRecord(runDir)
	if err != nil || a == nil {
		return false, err
	}
	return a.Trigger == TriggerServer && a.Conversation == r.Conversation && a.Receipt == r.Number, nil
}
