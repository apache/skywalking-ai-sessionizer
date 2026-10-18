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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"
)

// Decode reads JSON for a form, keeping every number as it was written, so
// that a form changes nothing the runtime sent.
func Decode(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// More is false at a closing bracket, so a value followed by one would
	// pass it; only the end of the input is the end.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("providerbody: more than one JSON value")
	}
	return v, nil
}

// Canonical writes a value Decode read as canonical JSON: object keys sorted
// by their UTF-8 bytes, no space between tokens, numbers as they were written,
// and in strings only what JSON requires escaped: the quotation mark, the
// backslash, and the characters below U+0020. Go's own encoder also
// escapes U+2028 and U+2029, which another implementation following the format
// page would not, so a form's digest is not left to it.
func Canonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := canonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func canonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(string(x))
	case string:
		quote(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := canonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			quote(buf, k)
			buf.WriteByte(':')
			if err := canonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("providerbody: %T is not a decoded JSON value", v)
	}
	return nil
}

func quote(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == '\n':
			buf.WriteString(`\n`)
		case c == '\r':
			buf.WriteString(`\r`)
		case c == '\t':
			buf.WriteString(`\t`)
		case c == '\b':
			buf.WriteString(`\b`)
		case c == '\f':
			buf.WriteString(`\f`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(hex[c>>4])
			buf.WriteByte(hex[c&0xf])
		case c < utf8.RuneSelf:
			buf.WriteByte(c)
		default:
			// Decode has already made the string valid UTF-8, so a run of
			// bytes is copied as it is.
			_, n := utf8.DecodeRuneInString(s[i:])
			buf.WriteString(s[i : i+n])
			i += n
			continue
		}
		i++
	}
	buf.WriteByte('"')
}
