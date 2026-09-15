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
	"errors"
)

// span is a half-open run [a, z) of a body's bytes.
type span struct{ a, z int }

// pieces finds the pieces of a body that is one valid JSON object: each
// element of the top-level "tools" array, and every string token of MinPiece
// bytes or more outside those elements. Spans come back in order and never
// overlap. It also returns the span of the first element of the top-level
// "messages" array, empty when there is none. The caller has checked that
// the body is valid JSON.
func pieces(b []byte) ([]span, span, error) {
	s := &scanner{b: b}
	i := s.ws(0)
	if i >= len(b) || b[i] != '{' {
		return nil, span{}, errors.New("the body is not a JSON object")
	}
	if _, err := s.value(i, true, false); err != nil {
		return nil, span{}, err
	}
	return s.spans, s.first, nil
}

type scanner struct {
	b     []byte
	spans []span
	first span
}

func (s *scanner) ws(i int) int {
	for i < len(s.b) {
		switch s.b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

var errShape = errors.New("the body ended inside a value")

// value reads the value at i and returns the index after it. top is true for
// the body's own object; whole is true when the value is a piece in itself
// and nothing inside it is cut.
func (s *scanner) value(i int, top, whole bool) (int, error) {
	if i >= len(s.b) {
		return 0, errShape
	}
	switch s.b[i] {
	case '{':
		return s.object(i, top, whole)
	case '[':
		return s.array(i, false, whole)
	case '"':
		z, err := s.str(i)
		if err != nil {
			return 0, err
		}
		if !whole && z-i >= MinPiece {
			s.spans = append(s.spans, span{i, z})
		}
		return z, nil
	default:
		// A number, true, false or null: runs to the next delimiter.
		z := i
		for z < len(s.b) {
			c := s.b[z]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			z++
		}
		return z, nil
	}
}

func (s *scanner) object(i int, top, whole bool) (int, error) {
	i = s.ws(i + 1)
	if i < len(s.b) && s.b[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(s.b) || s.b[i] != '"' {
			return 0, errShape
		}
		kz, err := s.str(i)
		if err != nil {
			return 0, err
		}
		tools := top && string(s.b[i:kz]) == `"tools"`
		messages := top && string(s.b[i:kz]) == `"messages"`
		i = s.ws(kz)
		if i >= len(s.b) || s.b[i] != ':' {
			return 0, errShape
		}
		i = s.ws(i + 1)
		switch {
		case tools && i < len(s.b) && s.b[i] == '[':
			i, err = s.array(i, true, whole)
		case messages && i < len(s.b) && s.b[i] == '[':
			if j := s.ws(i + 1); j < len(s.b) && s.b[j] != ']' {
				z, ferr := s.value(j, false, true)
				if ferr != nil {
					return 0, ferr
				}
				s.first = span{j, z}
			}
			i, err = s.array(i, false, whole)
		default:
			i, err = s.value(i, false, whole)
		}
		if err != nil {
			return 0, err
		}
		i = s.ws(i)
		if i >= len(s.b) {
			return 0, errShape
		}
		switch s.b[i] {
		case ',':
			i = s.ws(i + 1)
		case '}':
			return i + 1, nil
		default:
			return 0, errShape
		}
	}
}

// array reads an array. When each is true, every element is a piece whole.
func (s *scanner) array(i int, each, whole bool) (int, error) {
	i = s.ws(i + 1)
	if i < len(s.b) && s.b[i] == ']' {
		return i + 1, nil
	}
	for {
		start := i
		z, err := s.value(i, false, whole || each)
		if err != nil {
			return 0, err
		}
		if each && !whole {
			s.spans = append(s.spans, span{start, z})
		}
		i = s.ws(z)
		if i >= len(s.b) {
			return 0, errShape
		}
		switch s.b[i] {
		case ',':
			i = s.ws(i + 1)
		case ']':
			return i + 1, nil
		default:
			return 0, errShape
		}
	}
}

// str returns the index after the string token that starts at i.
func (s *scanner) str(i int) (int, error) {
	for j := i + 1; j < len(s.b); j++ {
		switch s.b[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, errShape
}
