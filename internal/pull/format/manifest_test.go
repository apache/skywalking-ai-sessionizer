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

package format_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
)

const (
	conv   = "c1"
	sess   = "s1"
	digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	other  = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// sample is a small valid manifest: two rounds, two landed files, a skeleton
// and two store entries.
func sample() *format.Manifest {
	return &format.Manifest{
		Conversation: conv, Session: sess,
		Head:    format.Head{Round: 2, Digest: digest, ThroughSeq: 3},
		Formats: format.Formats{SD: "sd/1", SF: "sf/1"},
		Files: []format.Entry{
			{File: "_conversations/c1/rounds/r000001-fedcba987654.sf", Kind: format.KindRound, Round: 1, Bytes: 10, Digest: other},
			{File: "_conversations/c1/rounds/r000002-0123456789ab.sf", Kind: format.KindRound, Round: 2, Bytes: 10, Digest: other},
			{File: "s1/provider/pieces/" + other + ".gz", Kind: format.KindStore, Bytes: 7, Digest: digest, Content: other},
			{File: "s1/provider/tips/1/" + digest + ".gz", Kind: format.KindStore, Bytes: 7, Digest: digest, Content: other},
			{File: "s1/provider_body/provider_body-20261025T103200.000000000Z-000003.sd", Kind: format.KindLanded, Seq: 3, Bytes: 9, Digest: other},
			{File: "s1/streams/main/transcript-20261025T103200.000000000Z-000001.sk", Kind: format.KindSkeleton, Seq: 1, Bytes: 9, Digest: other, LandedDigest: digest},
			{File: "s1/streams/main/transcript-20261025T103201.000000000Z-000002.sd", Kind: format.KindLanded, Seq: 2, Bytes: 9, Digest: other},
		},
	}
}

func encoded(t *testing.T, m *format.Manifest) string {
	t.Helper()
	var b bytes.Buffer
	if err := format.WriteManifest(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestAManifestRoundTrips(t *testing.T) {
	m := sample()
	got, err := format.ReadManifest(strings.NewReader(encoded(t, m)), conv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest() != m.Digest() || len(got.Files) != len(m.Files) || got.Head != m.Head || got.Session != sess {
		t.Fatalf("read back %+v", got)
	}
	if _, err := format.ReadManifest(strings.NewReader(encoded(t, m)), "c2"); !errors.Is(err, format.ErrManifest) {
		t.Fatalf("a manifest of another conversation: %v", err)
	}
}

// Members the schema does not know are allowed and passed over. Known ones
// can be neither null nor repeated, in any spelling, and a value after the
// object is refused.
func TestManifestMembersAreStrict(t *testing.T) {
	text := encoded(t, sample())
	cases := map[string]bool{
		strings.Replace(text, `{"schema"`, `{"later":{"a":[1,{"b":null}]},"schema"`, 1):                                                        true,
		strings.Replace(text, `"kind":"round",`, `"kind":"round","note":"x",`, 1):                                                              true,
		strings.Replace(text, `"generation":0`, `"generation":null`, 1):                                                                        false,
		strings.Replace(text, `"session":"s1"`, `"session":null`, 1):                                                                           false,
		strings.Replace(text, `"kind":"round","bytes":10,"digest":"`+other+`"`, `"kind":"round","bytes":10,"digest":null`, 1):                  false,
		strings.Replace(text, `"file":"_conversations`, `"file":null,"x":"_conversations`, 1):                                                  false,
		strings.Replace(text, `"generation":0`, `"generation":0,"generation":0`, 1):                                                            false,
		strings.Replace(text, `"generation":0`, `"generation":0,"generatio\u006e":0`, 1):                                                       false,
		strings.Replace(text, `"generation":0`, `"generation":-1`, 1):                                                                          false,
		strings.Replace(text, `"generation":0`, `"generation":1.0`, 1):                                                                         false,
		strings.Replace(text, `"generation":0`, `"generation":1e3`, 1):                                                                         false,
		strings.Replace(text, `"generation":0`, `"generation":9223372036854775808`, 1):                                                         false,
		strings.Replace(text, `,"generation":0`, ``, 1):                                                                                        false,
		strings.Replace(text, `"schema":"pull/1"`, `"schema":"pull/2"`, 1):                                                                     false,
		strings.Replace(text, `"sd":"sd/1"`, `"sd":"sd/2"`, 1):                                                                                 false,
		strings.Replace(text, `"kind":"round","bytes":10`, `"kind":"round","bytes":10,"seq":1`, 1):                                             false,
		strings.Replace(text, `"kind":"round","bytes":10`, `"kind":"rounds","bytes":10`, 1):                                                    false,
		strings.Replace(text, `"kind":"round","bytes":10,"digest":"`+other+`","round":1`, `"kind":"round","bytes":10,"digest":"`+other+`"`, 1): false,
		strings.Replace(text, `"bytes":10`, `"bytes":0`, 1):                                                                                    false,
		strings.Replace(text, `"bytes":10`, `"bytes":9223372036854775808`, 1):                                                                  false,
		strings.Replace(text, `"bytes":10`, `"bytes":"10"`, 1):                                                                                 false,
		strings.Replace(text, `"digest":"`+other+`"`, `"digest":"`+strings.ToUpper(other)+`"`, 1):                                              false,
		text + " ":         true,
		text + " {}":       false,
		text[:len(text)-3]: false,
	}
	for body, ok := range cases {
		_, err := format.ReadManifest(strings.NewReader(body), conv)
		if (err == nil) != ok {
			t.Errorf("error %v, want ok %v, for\n%s", err, ok, body)
		}
	}
}

// edited is sample with one change, written and read back.
func edited(t *testing.T, edit func(*format.Manifest)) error {
	t.Helper()
	m := sample()
	edit(m)
	_, err := format.ReadManifest(strings.NewReader(encoded(t, m)), conv)
	return err
}

func TestManifestNamesAreChecked(t *testing.T) {
	type edit struct {
		change func(*format.Manifest)
		// want is a piece of the refusal, so a case refused by another rule
		// than the one it is about fails.
		want string
	}
	round := func(name string) func(*format.Manifest) {
		return func(m *format.Manifest) { m.Files[0].File = "_conversations/c1/rounds/" + name }
	}
	for name, c := range map[string]edit{
		"unsorted": {func(m *format.Manifest) { m.Files[0], m.Files[1] = m.Files[1], m.Files[0] }, "does not sort after"},
		"repeated": {func(m *format.Manifest) { m.Files[1] = m.Files[0] }, "does not sort after"},
		"another conversation": {func(m *format.Manifest) {
			m.Files[0].File = "_conversations/c2/rounds/r000001-fedcba987654.sf"
		}, "is not a round of conversation c1"},
		"five digit round":     {round("r00001-fedcba987654.sf"), "a round is named"},
		"eleven hex round":     {round("r000001-fedcba98765.sf"), "a round is named"},
		"thirteen hex round":   {round("r000001-fedcba9876543.sf"), "a round is named"},
		"uppercase round":      {round("r000001-FEDCBA987654.sf"), "a round is named"},
		"round overflow":       {round("r18446744073709551616-fedcba987654.sf"), "round number"},
		"round number differs": {func(m *format.Manifest) { m.Files[0].Round = 3 }, "is named for round 1 and listed as round 3"},
		"head not named for its digest": {func(m *format.Manifest) {
			m.Files[1].File = "_conversations/c1/rounds/r000002-aaaaaaaaaaaa.sf"
		}, "is not named for the head digest"},
		"tip generation 0":         {func(m *format.Manifest) { m.Files[3].File = "s1/provider/tips/0/" + digest + ".gz" }, "is not a provider store tip or piece"},
		"tip generation 01":        {func(m *format.Manifest) { m.Files[3].File = "s1/provider/tips/01/" + digest + ".gz" }, "is not a provider store tip or piece"},
		"piece of another content": {func(m *format.Manifest) { m.Files[2].Content = digest }, "a piece holds what its name says"},
		"store name":               {func(m *format.Manifest) { m.Files[2].File = "s1/provider/pieces/x.gz" }, "is not a provider store tip or piece"},
		"landed outside a session directory": {func(m *format.Manifest) {
			m.Files[6].File = "s1/elsewhere/transcript-20261025T103201.000000000Z-000002.sd"
		}, "is not in a session's streams"},
		"landed sequence differs": {func(m *format.Manifest) { m.Files[6].Seq = 4 }, "is named for sequence 2 and listed as 4"},
		"unknown prefix": {func(m *format.Manifest) {
			m.Files[6].File = "s1/streams/main/notes-20261025T103201.000000000Z-000002.sd"
		}, "not a landed file name a producer writes"},
		"skeleton named sd": {func(m *format.Manifest) {
			m.Files[5].File = "s1/streams/main/transcript-20261025T103200.000000000Z-000001.sd"
		}, "a skeleton file ends in .sk"},
		"case alias of a directory": {func(m *format.Manifest) {
			m.Files[5].File = "s1/streams/Main/transcript-20261025T103200.000000000Z-000001.sk"
		}, "conflicting spellings"},
		"a session starting with _": {func(m *format.Manifest) {
			m.Session = "_s1"
			for i := range m.Files[2:] {
				m.Files[i+2].File = "_" + m.Files[i+2].File
			}
		}, "a session name cannot start with _"},
		"a reserved part": {func(m *format.Manifest) {
			m.Files[5].File = "s1/streams/.tmp-main/transcript-20261025T103200.000000000Z-000001.sk"
		}, ".tmp-"},
		"outside the session": {func(m *format.Manifest) {
			m.Files[6].File = "s2/streams/main/transcript-20261025T103201.000000000Z-000002.sd"
		}, "is outside session s1"},
		"a sequence missing": {func(m *format.Manifest) { m.Head.ThroughSeq = 4 }, "the head says 4"},
		"a sequence beyond":  {func(m *format.Manifest) { m.Head.ThroughSeq = 2 }, "the head says 2"},
		"a sequence twice": {func(m *format.Manifest) {
			m.Files[6].File, m.Files[6].Seq = "s1/streams/main/transcript-20261025T103201.000000000Z-000001.sd", 1
		}, "sequence 1 is listed twice"},
		"a round missing":       {func(m *format.Manifest) { m.Head.Round = 3 }, "the head says 3"},
		"empty evidence":        {func(m *format.Manifest) { m.Files = nil }, "it lists no file"},
		"head digest uppercase": {func(m *format.Manifest) { m.Head.Digest = format.CommitDigest(strings.ToUpper(digest)) }, "the head digest is not"},
	} {
		err := edited(t, c.change)
		if !errors.Is(err, format.ErrManifest) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, c.want)
		}
	}
}

// The digest is over the canonical file list: the order the entries were
// given in and members outside it do not change it; every listed field does.
func TestTheFileListDigestIsCanonical(t *testing.T) {
	m := sample()
	base := m.Digest()
	reordered := sample()
	reordered.Files[0], reordered.Files[6] = reordered.Files[6], reordered.Files[0]
	reordered.Generation = 9
	if reordered.Digest() != base {
		t.Fatal("the order of entries or the generation changed the digest")
	}
	var b bytes.Buffer
	if err := m.WriteFileList(&b); err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(b.String(), "\n", 2)[0]
	want := "_conversations/c1/rounds/r000001-fedcba987654.sf\tround\t-\t1\t10\t" + other + "\t-\t-"
	if first != want {
		t.Fatalf("first line %q, want %q", first, want)
	}
	for name, edit := range map[string]func(*format.Manifest){
		"bytes":         func(m *format.Manifest) { m.Files[0].Bytes++ },
		"digest":        func(m *format.Manifest) { m.Files[0].Digest = digest },
		"name":          func(m *format.Manifest) { m.Files[2].File = "s1/provider/pieces/" + digest + ".gz" },
		"landed digest": func(m *format.Manifest) { m.Files[5].LandedDigest = other },
		"content":       func(m *format.Manifest) { m.Files[3].Content = digest },
		"seq":           func(m *format.Manifest) { m.Files[4].Seq = 9 },
	} {
		changed := sample()
		edit(changed)
		if changed.Digest() == base {
			t.Errorf("%s did not change the digest", name)
		}
	}
}

func TestTheResponseCarriesItsTagAndLease(t *testing.T) {
	text := encoded(t, sample())
	withBoth := strings.Replace(text, `{"schema"`, `{"snapshot":"20261025T103200.123456789Z","lease":"`+digest+`","schema"`, 1)
	m, resp, err := format.ReadManifestResponse(strings.NewReader(withBoth), conv)
	if err != nil || resp.Snapshot != "20261025T103200.123456789Z" || resp.Lease != digest || m == nil {
		t.Fatalf("%+v %v", resp, err)
	}
	if _, _, err := format.ReadManifestResponse(strings.NewReader(text), conv); err == nil {
		t.Fatal("a response without its tag and lease was accepted")
	}
	for good, bad := range map[string]string{
		`"snapshot":"20261025T103200.123456789Z"`: `"snapshot":"20261025T103200Z"`,
		`"lease":"` + digest + `"`:                `"lease":"` + strings.ToUpper(digest) + `"`,
	} {
		body := strings.Replace(withBoth, good, bad, 1)
		if _, _, err := format.ReadManifestResponse(strings.NewReader(body), conv); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	// A saved manifest takes the two as unknown members.
	if _, err := format.ReadManifest(strings.NewReader(withBoth), conv); err != nil {
		t.Fatal(err)
	}
}

// Check holds a manifest built in memory to the same rules a parsed one
// meets, its order among them.
func TestCheckHoldsAManifestBuiltInMemory(t *testing.T) {
	m := sample()
	if err := m.Check(conv); err != nil {
		t.Fatal(err)
	}
	m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
	if err := m.Check(conv); !errors.Is(err, format.ErrManifest) || !strings.Contains(err.Error(), "does not sort after") {
		t.Fatalf("an unsorted manifest: %v", err)
	}
	m = sample()
	m.Files[6].Bytes = 0
	if err := m.Check(conv); !errors.Is(err, format.ErrManifest) || !strings.Contains(err.Error(), "empty evidence") {
		t.Fatalf("an empty file: %v", err)
	}
	// A member of another kind is refused in memory as it is in JSON, where
	// writing would drop it without a word.
	for name, edit := range map[string]func(*format.Manifest){
		"a round with a seq":         func(m *format.Manifest) { m.Files[0].Seq = 1 },
		"a landed file with a round": func(m *format.Manifest) { m.Files[6].Round = 1 },
		"a landed file with a landed digest": func(m *format.Manifest) {
			m.Files[6].LandedDigest = other
		},
		"a skeleton with content":  func(m *format.Manifest) { m.Files[5].Content = other },
		"a store entry with a seq": func(m *format.Manifest) { m.Files[2].Seq = 1 },
	} {
		m := sample()
		edit(m)
		if err := m.Check(conv); !errors.Is(err, format.ErrManifest) || !strings.Contains(err.Error(), "entry has no") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A landed name is one storage reads: a sequence in more digits than storage
// writes is the same sequence, and two spellings of it are two entries for
// one sequence.
func TestLandedNamesFollowStoragesGrammar(t *testing.T) {
	if err := edited(t, func(m *format.Manifest) {
		m.Files[6].File = "s1/streams/main/transcript-20261025T103201.000000000Z-0000002.sd"
	}); err != nil {
		t.Fatalf("a sequence in seven digits: %v", err)
	}
	err := edited(t, func(m *format.Manifest) {
		m.Files[6].File, m.Files[6].Seq = "s1/streams/main/transcript-20261025T103201.000000000Z-0000001.sd", 1
	})
	if !errors.Is(err, format.ErrManifest) || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("one sequence in two spellings: %v", err)
	}
}
