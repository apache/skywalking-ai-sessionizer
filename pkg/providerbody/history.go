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

package providerbody

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// What a body says about the history its call was sent, so the assembler can
// tell where a call's history changed without reading a body.
//
// A request's messages are written as a list of digests, one per message, in
// the form its runtime's adapter defines; a response's reply as one more. Two
// lists compare only under one form, and a form's rules change only with a new
// version, so a digest that differs always means the recorded message did.

// The forms an adapter writes. The rules of each are in docs/en/formats/session-data.md.
const (
	FormClaudeCode = "claude-code/1"
	FormLangChain  = "langchain/1"
)

// Why a list or a reply is unavailable.
const (
	// UnrecognisedShape is a request or response the form does not cover.
	UnrecognisedShape = "unrecognised_shape"
	// PluralPrompts is a LangChain request that is not exactly one prompt.
	PluralPrompts = "plural_prompts"
	// TooLarge is a list over the cap: what it would take is more than a
	// quarter of the budget a landed file has.
	TooLarge = "too_large"
	// NoSingleReply is a response that is not exactly one complete message.
	NoSingleReply = "no_single_reply"
	// NoRequest is a LangChain response whose request did not land, so
	// whether it answered exactly one prompt is not known.
	NoRequest = "no_request"
)

// Thread types a Claude Code request carries.
const (
	ThreadCreate   = "create"
	ThreadContinue = "continue"
)

// MaxPrevious bounds the message id a continue request names. A provider's
// message id is a few dozen bytes; anything longer is not one.
const MaxPrevious = 128

// Entry is one message: the digest of the id its runtime sent, when it sent
// one, and the digest of the message's form. Both are the first sixteen
// hexadecimal characters of a SHA-256, so an entry has one size whatever the
// message holds.
type Entry struct {
	ID     string `json:"id,omitempty"`
	Digest string `json:"digest"`
}

// Messages is a request's message list: whole, or the first Keep entries of
// the list of the body its copy came from followed by Add, or unavailable
// with the reason and, for a list too large, how many messages it holds.
type Messages struct {
	Entries     []Entry
	Base        string
	Keep        int
	Add         []Entry
	Unavailable string
	Count       int
}

// Relative says the list is written against a base.
func (m *Messages) Relative() bool { return m.Base != "" }

// MarshalJSON writes the one shape the list has, so an empty whole list reads
// as a request with no messages, never as an absent one.
func (m Messages) MarshalJSON() ([]byte, error) {
	switch {
	case m.Unavailable != "":
		return json.Marshal(struct {
			Unavailable string `json:"unavailable"`
			Count       int    `json:"count,omitempty"`
		}{m.Unavailable, m.Count})
	case m.Base != "":
		return json.Marshal(struct {
			Base string  `json:"base"`
			Keep int     `json:"keep"`
			Add  []Entry `json:"add"`
		}{m.Base, m.Keep, nonNil(m.Add)})
	}
	return json.Marshal(struct {
		Entries []Entry `json:"entries"`
	}{nonNil(m.Entries)})
}

// UnmarshalJSON reads one of the three shapes. Anything else reads as a list
// unavailable, unrecognised_shape, never as an error: the manifest still joins
// its body to its call, and a reader compares nothing it cannot read.
func (m *Messages) UnmarshalJSON(b []byte) error {
	if err := m.read(b); err != nil {
		*m = Messages{Unavailable: UnrecognisedShape}
	}
	return nil
}

func (m *Messages) read(b []byte) error {
	var raw struct {
		Entries     *[]Entry `json:"entries"`
		Base        *string  `json:"base"`
		Keep        *int     `json:"keep"`
		Add         *[]Entry `json:"add"`
		Unavailable *string  `json:"unavailable"`
		Count       int      `json:"count"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*m = Messages{}
	switch {
	case raw.Unavailable != nil && raw.Entries == nil && raw.Base == nil:
		if *raw.Unavailable == "" || raw.Count < 0 || raw.Count > math.MaxUint32 {
			return errors.New("providerbody: an unavailable list names no reason, or a count no list holds")
		}
		m.Unavailable, m.Count = *raw.Unavailable, raw.Count
	case raw.Base != nil && raw.Keep != nil && raw.Add != nil && raw.Entries == nil && raw.Unavailable == nil:
		if *raw.Base == "" || *raw.Keep < 1 || *raw.Keep > math.MaxUint32 {
			return errors.New("providerbody: a list against a base names no base or keeps nothing")
		}
		m.Base, m.Keep, m.Add = *raw.Base, *raw.Keep, *raw.Add
	case raw.Entries != nil && raw.Base == nil && raw.Unavailable == nil:
		m.Entries = *raw.Entries
	default:
		return errors.New("providerbody: a message list is not exactly one of entries, base or unavailable")
	}
	for _, list := range [][]Entry{m.Entries, m.Add} {
		for _, e := range list {
			if !isDigest(e.Digest) || (e.ID != "" && !isDigest(e.ID)) {
				return fmt.Errorf("providerbody: a message entry is not two digests: %q %q", e.ID, e.Digest)
			}
		}
	}
	return nil
}

// Reply is the message a response produced, or why there is none to name.
type Reply struct {
	ID          string `json:"id,omitempty"`
	Digest      string `json:"digest,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}

// Thread is the provider thread a Claude Code request starts or continues.
type Thread struct {
	Type string `json:"type"`
	// Previous is the provider's id of the message a continue request goes
	// on from, as sent.
	Previous string `json:"previous,omitempty"`
}

// History is what an adapter read from one body about its call's history.
type History struct {
	Form string
	// Messages is a request's whole list, nil when Unavailable says why.
	Messages    []Entry
	Unavailable string
	// System is the digest of a Claude Code request's system prompt without
	// its billing header; empty when it carries nothing else.
	System string
	Thread *Thread
	// Reply is a response's message.
	Reply *Reply
}

// EntryOf is the entry of a message whose form is form, in canonical JSON,
// and whose id is id when hasID says the runtime sent one.
func EntryOf(id string, hasID bool, form []byte) Entry {
	e := Entry{Digest: short(form)}
	if hasID {
		e.ID = short([]byte(id))
	}
	return e
}

func short(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func isDigest(s string) bool {
	if len(s) != 16 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func nonNil(e []Entry) []Entry {
	if e == nil {
		return []Entry{}
	}
	return e
}

// writeHistory puts what the adapter read into the manifest. A request's list
// is written against the body its copy came from when that body's list is in
// the same form and the two share a front, so a list's chain is the body's
// chain and never deeper; otherwise it is written whole. A list that would
// take more than a quarter of the budget is unavailable as too large, since
// the budget never takes a manifest out.
func (s *Session) writeHistory(m *Manifest, h *History) {
	m.HistoryForm, m.Messages, m.System, m.Thread, m.Reply = "", nil, "", nil, nil
	if h == nil || h.Form == "" {
		return
	}
	m.HistoryForm, m.System, m.Thread, m.Reply = h.Form, h.System, h.Thread, h.Reply
	if m.Role != RoleRequest {
		return
	}
	if h.Unavailable != "" {
		m.Messages = &Messages{Unavailable: h.Unavailable}
		return
	}
	list := &Messages{Entries: nonNil(h.Messages)}
	if base := copiedFrom(m); base != "" {
		if prior, form, err := s.ListOf(base); err == nil && form == h.Form {
			if keep := sharedFront(prior, h.Messages); keep > 0 {
				list = &Messages{Base: base, Keep: keep, Add: nonNil(h.Messages[keep:])}
			}
		}
	}
	if s.budget > 0 {
		written := list.Entries
		if list.Relative() {
			written = list.Add
		}
		if b, err := json.Marshal(written); err == nil && int64(len(b)) > s.budget/4 {
			list = &Messages{Unavailable: TooLarge, Count: len(h.Messages)}
		}
	}
	m.Messages = list
}

// ListOf is the whole message list of the request held under id, and its
// form. It fails for a body that carries none, one whose list is unavailable,
// and one written against a base that does not hold: a base that is not a
// request added before it, or of another form, or shorter than what is kept
// from it.
func (s *Session) ListOf(id string) ([]Entry, string, error) {
	return s.listOf(id, 0)
}

func (s *Session) listOf(id string, depth int) ([]Entry, string, error) {
	h, ok := s.records[id]
	switch {
	case !ok:
		return nil, "", fmt.Errorf("providerbody: no body %s", id)
	case h.m.Role != RoleRequest || h.m.Messages == nil || h.m.HistoryForm == "":
		return nil, "", fmt.Errorf("providerbody: %s carries no message list", id)
	case h.m.Messages.Unavailable != "":
		return nil, "", fmt.Errorf("providerbody: the message list of %s is unavailable: %s", id, h.m.Messages.Unavailable)
	case !h.m.Messages.Relative():
		return h.m.Messages.Entries, h.m.HistoryForm, nil
	case depth >= MaxDepth:
		return nil, "", fmt.Errorf("providerbody: the message list of %s is more than %d bases deep", id, MaxDepth)
	}
	l := h.m.Messages
	if base, ok := s.records[l.Base]; !ok || base.at >= h.at {
		return nil, "", fmt.Errorf("providerbody: %s keeps entries of %s, which was not added before it", id, l.Base)
	}
	prior, form, err := s.listOf(l.Base, depth+1)
	if err != nil {
		return nil, "", err
	}
	if form != h.m.HistoryForm || l.Keep > len(prior) {
		return nil, "", fmt.Errorf("providerbody: %s keeps %d entries of a %s list of %d", id, l.Keep, form, len(prior))
	}
	out := make([]Entry, 0, l.Keep+len(l.Add))
	return append(append(out, prior[:l.Keep]...), l.Add...), form, nil
}

// copiedFrom is the body a manifest's copy segment names, or "".
func copiedFrom(m *Manifest) string {
	for _, seg := range m.Segments {
		if seg.Copy != nil {
			return seg.Copy.From
		}
	}
	return ""
}

// sharedFront is how many entries two lists share from the start, exactly:
// the same id or none, and the same digest.
func sharedFront(a, b []Entry) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}
