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

// Package boundedjson limits memory before decoding untrusted records.
// Schemas belong to callers. Unknown values pass through the same checks.
package boundedjson

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf16"
	"unicode/utf8"
)

// The limits that keep a record's memory bounded before any schema reads it.
const (
	MaxDepth       = 16
	MaxMembers     = 4096
	MaxStringBytes = 64 * 1024
)

// Kind is what a token is: a container's start or end, a member name, or a value.
type Kind byte

const (
	Object    Kind = '{'
	ObjectEnd Kind = '}'
	Array     Kind = '['
	ArrayEnd  Kind = ']'
	Name      Kind = ':'
	String    Kind = '"'
	Number    Kind = '0'
	True      Kind = 't'
	False     Kind = 'f'
	Null      Kind = 'n'
)

// Token is one piece of the value. A number keeps only its checked conversion
// state, since an unknown number can be arbitrarily long.
type Token struct {
	Kind Kind
	Text string

	unsigned uint64
	whole    bool
	overflow bool
}

// Uint64 is a number token's value. A fraction, an exponent, a minus sign or a
// value past the unsigned 64-bit range is refused.
func (t Token) Uint64() (uint64, error) {
	if t.Kind != Number || !t.whole || t.overflow {
		return 0, errors.New("boundedjson: expected an unsigned 64-bit decimal integer")
	}
	return t.unsigned, nil
}

// Counts must fit signed offsets without accepting negative values.
func (t Token) Int64() (int64, error) {
	n, err := t.Uint64()
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt64 {
		return 0, errors.New("boundedjson: integer exceeds the signed 64-bit range")
	}
	return int64(n), nil
}

type container struct {
	kind  Kind
	state byte
	names map[string]struct{}
}

// Scanner reads one JSON value as tokens, checking every limit and refusing
// duplicate member names as it goes.
type Scanner struct {
	reader  *bufio.Reader
	stack   []container
	started bool
	err     error
}

// New scans the value reader holds.
func New(reader io.Reader) *Scanner {
	return &Scanner{reader: bufio.NewReader(reader)}
}

// Next returns end of input only after one complete value and its trailing whitespace.
// Callers must reach that end to detect trailing values and delayed read failures.
func (s *Scanner) Next() (Token, error) {
	return s.next(true)
}

// Skip consumes one value without retaining its strings. Object names still need
// duplicate checks, including names inside unknown values.
func (s *Scanner) Skip() error {
	token, err := s.next(false)
	if err != nil {
		return err
	}
	if token.Kind == Name || token.Kind == ObjectEnd || token.Kind == ArrayEnd {
		s.err = errors.New("boundedjson: expected a value to skip")
		return s.err
	}
	depth := 0
	for {
		switch token.Kind {
		case Object, Array:
			depth++
		case ObjectEnd, ArrayEnd:
			depth--
		}
		if depth == 0 {
			return nil
		}
		token, err = s.next(false)
		if err != nil {
			return err
		}
	}
}

func (s *Scanner) next(keep bool) (token Token, err error) {
	if s.err != nil {
		return Token{}, s.err
	}
	defer func() { s.err = err }()
	for {
		b, err := s.nonSpace()
		if len(s.stack) == 0 {
			if s.started {
				if errors.Is(err, io.EOF) {
					return Token{}, io.EOF
				}
				if err != nil {
					return Token{}, err
				}
				return Token{}, errors.New("boundedjson: trailing value")
			}
			if err != nil {
				return Token{}, required(err)
			}
			s.started = true
			return s.value(b, keep)
		}
		if err != nil {
			return Token{}, required(err)
		}
		frame := &s.stack[len(s.stack)-1]
		if frame.state == 3 {
			if (frame.kind == Object && b == '}') || (frame.kind == Array && b == ']') {
				return s.end(b), nil
			}
			if b != ',' {
				return Token{}, errors.New("boundedjson: expected a comma or container end")
			}
			frame.state = 1
			s.consume()
			continue
		}
		if frame.kind == Object {
			if frame.state == 2 {
				if b != ':' {
					return Token{}, errors.New("boundedjson: expected a colon")
				}
				s.consume()
				frame.state = 3
				b, err = s.nonSpace()
				if err != nil {
					return Token{}, required(err)
				}
				return s.value(b, keep)
			}
			if b == '}' && frame.state == 0 {
				return s.end(b), nil
			}
			if b != '"' {
				return Token{}, errors.New("boundedjson: expected a member name")
			}
			if len(frame.names) >= MaxMembers {
				return Token{}, errors.New("boundedjson: object exceeds 4096 members")
			}
			name, err := s.string(true)
			if err != nil {
				return Token{}, err
			}
			if _, exists := frame.names[name]; exists {
				return Token{}, fmt.Errorf("boundedjson: duplicate member %q", name)
			}
			frame.names[name] = struct{}{}
			frame.state = 2
			return Token{Kind: Name, Text: name}, nil
		}
		if b == ']' && frame.state == 0 {
			return s.end(b), nil
		}
		frame.state = 3
		return s.value(b, keep)
	}
}

// consume reads a byte that peek has already buffered, so it cannot fail.
func (s *Scanner) consume() { _, _ = s.reader.ReadByte() }

func (s *Scanner) end(b byte) Token {
	s.consume()
	last := len(s.stack) - 1
	// A finished object's names must not stay alive in the stack's backing array.
	s.stack[last] = container{}
	s.stack = s.stack[:last]
	return Token{Kind: Kind(b)}
}

func (s *Scanner) value(b byte, keep bool) (Token, error) {
	switch b {
	case '{', '[':
		if len(s.stack) >= MaxDepth {
			return Token{}, errors.New("boundedjson: nesting exceeds 16 containers")
		}
		s.consume()
		frame := container{kind: Kind(b)}
		if b == '{' {
			frame.names = make(map[string]struct{})
		}
		s.stack = append(s.stack, frame)
		return Token{Kind: Kind(b)}, nil
	case '"':
		value, err := s.string(keep)
		return Token{Kind: String, Text: value}, err
	case 't', 'f', 'n':
		word := map[byte]string{'t': "true", 'f': "false", 'n': "null"}[b]
		for i := range word {
			got, err := s.reader.ReadByte()
			if err != nil {
				return Token{}, required(err)
			}
			if got != word[i] {
				return Token{}, errors.New("boundedjson: invalid literal")
			}
		}
		return Token{Kind: Kind(b)}, nil
	default:
		if b == '-' || digit(b) {
			return s.number()
		}
		return Token{}, errors.New("boundedjson: expected a value")
	}
}

func (s *Scanner) string(keep bool) (string, error) {
	s.consume()
	var value []byte
	size := 0
	for {
		r, width, err := s.reader.ReadRune()
		if err != nil {
			return "", required(err)
		}
		if r == '"' {
			return string(value), nil
		}
		if r < 0x20 || (r == utf8.RuneError && width == 1) {
			return "", errors.New("boundedjson: invalid string encoding")
		}
		if r == '\\' {
			b, err := s.reader.ReadByte()
			if err != nil {
				return "", required(err)
			}
			switch b {
			case '"', '\\', '/':
				r = rune(b)
			case 'b':
				r = '\b'
			case 'f':
				r = '\f'
			case 'n':
				r = '\n'
			case 'r':
				r = '\r'
			case 't':
				r = '\t'
			case 'u':
				r, err = s.hexRune()
				if err != nil {
					return "", err
				}
				if utf16.IsSurrogate(r) {
					if r < 0xd800 || r > 0xdbff {
						return "", errors.New("boundedjson: unpaired surrogate")
					}
					for _, want := range []byte{'\\', 'u'} {
						got, err := s.reader.ReadByte()
						if err != nil {
							return "", required(err)
						}
						if got != want {
							return "", errors.New("boundedjson: unpaired surrogate")
						}
					}
					low, err := s.hexRune()
					if err != nil {
						return "", err
					}
					if low < 0xdc00 || low > 0xdfff {
						return "", errors.New("boundedjson: unpaired surrogate")
					}
					r = utf16.DecodeRune(r, low)
				}
			default:
				return "", errors.New("boundedjson: invalid string escape")
			}
		}
		width = utf8.RuneLen(r)
		if size > MaxStringBytes-width {
			return "", errors.New("boundedjson: string exceeds 65536 decoded bytes")
		}
		size += width
		if keep {
			if size > cap(value) {
				capacity := min(MaxStringBytes, max(size, max(64, cap(value)*2)))
				next := make([]byte, len(value), capacity)
				copy(next, value)
				value = next
			}
			value = utf8.AppendRune(value, r)
		}
	}
}

func (s *Scanner) hexRune() (rune, error) {
	var value rune
	for range 4 {
		b, err := s.reader.ReadByte()
		if err != nil {
			return 0, required(err)
		}
		value <<= 4
		switch {
		case digit(b):
			value += rune(b - '0')
		case b >= 'a' && b <= 'f':
			value += rune(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value += rune(b-'A') + 10
		default:
			return 0, errors.New("boundedjson: invalid hexadecimal escape")
		}
	}
	return value, nil
}

func (s *Scanner) number() (Token, error) {
	token := Token{Kind: Number, whole: true}
	b, err := s.peek()
	if b == '-' {
		token.whole = false
		s.consume()
		b, err = s.peek()
	}
	if err != nil {
		return Token{}, required(err)
	}
	if !digit(b) {
		return Token{}, errors.New("boundedjson: expected a number digit")
	}
	zero := b == '0'
	for digit(b) && err == nil {
		s.consume()
		d := uint64(b - '0')
		if token.unsigned > (math.MaxUint64-d)/10 {
			token.overflow = true
		} else if !token.overflow {
			token.unsigned = token.unsigned*10 + d
		}
		b, err = s.peek()
		if zero {
			break
		}
	}
	if zero && err == nil && digit(b) {
		return Token{}, errors.New("boundedjson: number has a leading zero")
	}
	if err == nil && b == '.' {
		token.whole = false
		s.consume()
		if err := s.digits(); err != nil {
			return Token{}, err
		}
		b, err = s.peek()
	}
	if err == nil && (b == 'e' || b == 'E') {
		token.whole = false
		s.consume()
		b, err = s.peek()
		if err != nil {
			return Token{}, required(err)
		}
		if b == '+' || b == '-' {
			s.consume()
		}
		if err := s.digits(); err != nil {
			return Token{}, err
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return Token{}, err
	}
	return token, nil
}

func (s *Scanner) digits() error {
	b, err := s.peek()
	if err != nil {
		return required(err)
	}
	if !digit(b) {
		return errors.New("boundedjson: expected a number digit")
	}
	for err == nil && digit(b) {
		s.consume()
		b, err = s.peek()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func digit(b byte) bool { return b >= '0' && b <= '9' }

func (s *Scanner) nonSpace() (byte, error) {
	for {
		b, err := s.peek()
		if err != nil {
			return 0, err
		}
		switch b {
		case ' ', '\t', '\n', '\r':
			s.consume()
		default:
			return b, nil
		}
	}
}

func (s *Scanner) peek() (byte, error) {
	b, err := s.reader.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func required(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
