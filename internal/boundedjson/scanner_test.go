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

package boundedjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"testing/iotest"
)

func scan(text string) error {
	scanner := New(strings.NewReader(text))
	for {
		_, err := scanner.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func TestLimits(t *testing.T) {
	object := func(count int) string {
		var out strings.Builder
		out.WriteByte('{')
		for i := range count {
			if i > 0 {
				out.WriteByte(',')
			}
			fmt.Fprintf(&out, `"name%d":null`, i)
		}
		out.WriteByte('}')
		return out.String()
	}
	for _, tc := range []struct{ name, good, bad, message string }{
		{"containers", strings.Repeat(`[{"a":`, 8) + `0` + strings.Repeat(`}]`, 8), strings.Repeat(`[{"a":`, 8) + `[0]` + strings.Repeat(`}]`, 8), "nesting"},
		{"members", object(MaxMembers), object(MaxMembers + 1), "members"},
		{"string", `"` + strings.Repeat("a", MaxStringBytes) + `"`, `"` + strings.Repeat("a", MaxStringBytes+1) + `"`, "decoded bytes"},
		{"name", `{"` + strings.Repeat("a", MaxStringBytes) + `":0}`, `{"` + strings.Repeat("a", MaxStringBytes+1) + `":0}`, "decoded bytes"},
		{"escaped bytes", `"` + strings.Repeat(`\u00e9`, MaxStringBytes/2) + `"`, `"` + strings.Repeat(`\u00e9`, MaxStringBytes/2) + `x"`, "decoded bytes"},
		{"surrogate bytes", `"` + strings.Repeat(`\ud83d\ude00`, MaxStringBytes/4) + `"`, `"` + strings.Repeat(`\ud83d\ude00`, MaxStringBytes/4) + `x"`, "decoded bytes"},
		{"encoded bytes", `"` + strings.Repeat("é", MaxStringBytes/2) + `"`, `"` + strings.Repeat("é", MaxStringBytes/2) + `x"`, "decoded bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := scan(tc.good); err != nil {
				t.Fatalf("boundary: %v", err)
			}
			if err := scan(tc.bad); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("one beyond boundary: %v, want %s", err, tc.message)
			}
			scanner := New(strings.NewReader(`{"unknown":` + tc.bad + `}`))
			scanner.Next()
			scanner.Next()
			if err := scanner.Skip(); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("unknown value: %v, want %s", err, tc.message)
			}
		})
	}
}

func TestDuplicateNames(t *testing.T) {
	for _, text := range []string{
		`{"a":1,"a":2}`, `{"a":1,"\u0061":2}`, `{"é":1,"\u00e9":2}`,
		`{"😀":1,"\ud83d\ude00":2}`, `{"unknown":{"a":1,"a":2}}`,
		`[{"a":1,"a":2}]`,
	} {
		if err := scan(text); err == nil || !strings.Contains(err.Error(), "duplicate member") {
			t.Errorf("%s: %v", text, err)
		}
		if err := New(strings.NewReader(text)).Skip(); err == nil {
			t.Errorf("skipped duplicate: %s", text)
		}
	}
	var out strings.Builder
	out.WriteString(`{"last":0`)
	for i := range MaxMembers - 2 {
		fmt.Fprintf(&out, `,"n%d":0`, i)
	}
	out.WriteString(`,"\u006cast":1}`)
	if err := scan(out.String()); err == nil || !strings.Contains(err.Error(), "duplicate member") {
		t.Fatalf("last member escaped duplicate: %v", err)
	}
	if err := scan(`{"a":{"a":1},"A":2,"b":[{"a":1},{"a":2}]}`); err != nil {
		t.Fatal(err)
	}
}

func TestFinishedObjectsReleaseNames(t *testing.T) {
	scanner := New(strings.NewReader(`[{"a":1},{"a":2}]`))
	for {
		token, err := scanner.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if token.Kind == ObjectEnd {
			for _, frame := range scanner.stack[len(scanner.stack):cap(scanner.stack)] {
				if frame.names != nil {
					t.Fatal("finished object retained its names")
				}
			}
		}
	}
}

func TestStringEncoding(t *testing.T) {
	for _, text := range []string{
		`"\ud800"`, `"\udc00"`, `"\udc00\udc00"`, `"\ud800\vdc00"`, `"\ud800\u0041"`, `"\ud800\ud800"`,
		`"\ud800x"`, `"\ud800\x1234"`, `"\ud800\u"`, `"\u123"`, `"\uZZZZ"`, `"\q"`,
		"\"\x00\"", "\"\x1f\"", "\"\xff\"", "\"\xc0\x80\"", "\"\xed\xa0\x80\"", "\"\xe2\x82\"",
	} {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			if err := scan(text); err == nil {
				t.Fatal("accepted invalid encoding")
			}
			if err := scan(`{` + text + `:0}`); err == nil {
				t.Fatal("accepted invalid name")
			}
			if err := New(strings.NewReader(`{"unknown":` + text + `}`)).Skip(); err == nil {
				t.Fatal("skipped invalid encoding")
			}
		})
	}
	text := `"\"\\\/\b\f\n\r\t\u0000\uD83D\uDE00�"`
	token, err := New(iotest.OneByteReader(strings.NewReader(text))).Next()
	if err != nil {
		t.Fatal(err)
	}
	if token.Text != "\"\\/\b\f\n\r\t\x00😀�" {
		t.Fatalf("decoded %q", token.Text)
	}
}

func TestSyntax(t *testing.T) {
	for _, text := range []string{
		"", " ", "{", "[", `"`, `"x`, `"\`, `tru`, `fals`, `nul`, `trux`,
		`{"a"}`, `{"a":}`, `{"a" 1}`, `{"a"x1}`, `{"a":1 "b":2}`, `{"a":1,}`, `{1:2}`,
		`[1,]`, `[1 2]`, `[}`, `[,1]`, `true false`, `{} []`, `0x1`, `+1`, `.1`,
		`01`, `-01`, `-`, `-.1`, `1.`, `1.e2`, `1e`, `1e+`, `1e-`, `1e.2`, "\ufeff{}", "true\v",
	} {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			if err := scan(text); err == nil {
				t.Fatal("accepted malformed input")
			}
		})
	}
	for _, text := range []string{`{}`, `[]`, `true`, `false`, `null`, `-0`, `-12.50e+123`, `0E-1`, " \t\r\n[{}, [], 1, false, null] \n"} {
		if err := scan(text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
}

func TestCheckedNumbers(t *testing.T) {
	for _, tc := range []struct {
		text  string
		want  uint64
		valid bool
	}{
		{"0", 0, true}, {"18446744073709551615", math.MaxUint64, true},
		{"18446744073709551616", 0, false}, {"184467440737095516150", 0, false},
		{"-0", 0, false}, {"1.0", 0, false}, {"1e0", 0, false}, {`"1"`, 0, false},
	} {
		token, err := New(strings.NewReader(tc.text)).Next()
		if err != nil {
			t.Fatal(err)
		}
		got, err := token.Uint64()
		if (err == nil) != tc.valid || got != tc.want {
			t.Errorf("%s: %d, %v", tc.text, got, err)
		}
	}
	token, err := New(strings.NewReader("9223372036854775807")).Next()
	if err != nil {
		t.Fatal(err)
	}
	if value, err := token.Int64(); err != nil || value != math.MaxInt64 {
		t.Fatalf("signed boundary: %d, %v", value, err)
	}
	// The token must scan, so only Int64 can refuse it.
	token, err = New(strings.NewReader("9223372036854775808")).Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := token.Int64(); err == nil {
		t.Fatal("signed overflow accepted")
	}
}

type repeatedByte byte

func (b repeatedByte) Read(out []byte) (int, error) {
	for i := range out {
		out[i] = byte(b)
	}
	return len(out), nil
}

type countedReader struct {
	io.Reader
	count int
}

func (r *countedReader) Read(out []byte) (int, error) {
	n, err := r.Reader.Read(out)
	r.count += n
	return n, err
}

func TestStreaming(t *testing.T) {
	reader := io.MultiReader(io.LimitReader(repeatedByte(' '), 2<<20), strings.NewReader(`{"unknown":[`),
		io.LimitReader(repeatedByte('9'), 2<<20), strings.NewReader(`,"`),
		io.LimitReader(repeatedByte('x'), MaxStringBytes), strings.NewReader(`"]}`), io.LimitReader(repeatedByte('\n'), 2<<20))
	scanner := New(reader)
	if err := scanner.Skip(); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
	reader2 := &countedReader{Reader: io.MultiReader(strings.NewReader(`"`), repeatedByte('x'))}
	if _, err := New(reader2).Next(); err == nil {
		t.Fatal("accepted unlimited string")
	}
	if reader2.count > 1+MaxStringBytes+4096 {
		t.Fatalf("read %d bytes before refusal", reader2.count)
	}
	scanner = New(strings.NewReader(`"` + strings.Repeat("x", MaxStringBytes) + `"`))
	token, err := scanner.next(false)
	if err != nil || token.Text != "" {
		t.Fatalf("skipped string retained: %d, %v", len(token.Text), err)
	}
}

type readFailure struct {
	err      error
	returned bool
}

func (r *readFailure) Read([]byte) (int, error) {
	if r.returned {
		return 0, io.EOF
	}
	r.returned = true
	return 0, r.err
}

func TestReadErrors(t *testing.T) {
	failure := errors.New("read failed")
	for _, text := range []string{``, `1`, `1.2`, `1e3`, `1e`, `"x`, `{"a":`, `true`, `[] `} {
		scanner := New(io.MultiReader(strings.NewReader(text), &readFailure{err: failure}))
		for {
			_, err := scanner.Next()
			if err == nil {
				continue
			}
			if !errors.Is(err, failure) {
				t.Fatalf("%q: %v", text, err)
			}
			if _, again := scanner.Next(); !errors.Is(again, failure) {
				t.Fatalf("lost failure: %v", again)
			}
			break
		}
	}
}

func TestSkipRequiresValue(t *testing.T) {
	for _, text := range []string{`{"a":0}`, `[]`, `{}`} {
		scanner := New(strings.NewReader(text))
		scanner.Next()
		if err := scanner.Skip(); err == nil {
			t.Fatalf("skipped a name or end: %s", text)
		}
	}
}

func TestScannerFailureStaysTerminal(t *testing.T) {
	scanner := New(strings.NewReader(`[1,]`))
	for {
		_, err := scanner.Next()
		if err == nil {
			continue
		}
		//nolint:errorlint // the same error value, not one that wraps it
		if _, again := scanner.Next(); again != err {
			t.Fatalf("failure changed: %v, then %v", err, again)
		}
		//nolint:errorlint // the same error value, not one that wraps it
		if again := scanner.Skip(); again != err {
			t.Fatalf("skip resumed failed scanner: %v", again)
		}
		break
	}
}

func TestNumberTokenSyntax(t *testing.T) {
	for _, text := range []string{"01", "-01", "-", "1.", "1e", "1e+", "1e-"} {
		if _, err := New(strings.NewReader(text)).Next(); err == nil {
			t.Fatalf("returned invalid number token %q", text)
		}
	}
}

func FuzzScannerAcceptance(f *testing.F) {
	for _, seed := range []string{`{"files":[{"name":"one","bytes":1}],"unknown":[null,true,1.2e-3]}`, `{"a":1,"a":2}`, `"\ud83d\ude00"`, `[1,]`, `1e`, `{"a"x1}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if err := scan(text); err == nil && !json.Valid([]byte(text)) {
			t.Fatalf("accepted invalid input %q", text)
		}
	})
}
