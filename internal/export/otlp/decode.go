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
	"encoding/binary"
	"errors"
	"fmt"
)

// Decode reads what Encode writes: the OTLP logs request as this package
// sends it, back into the same values. It exists for the receiver in the
// scenario checks, which verify every request against the export page, and
// it reads only the fields the sender writes.
func Decode(b []byte) ([]ResourceLogs, error) {
	var out []ResourceLogs
	err := each(b, func(field int, wire int, v []byte, _ uint64) error {
		if field != 1 || wire != 2 {
			return nil
		}
		g, err := decodeResourceLogs(v)
		if err != nil {
			return err
		}
		out = append(out, g)
		return nil
	})
	return out, err
}

func decodeResourceLogs(b []byte) (ResourceLogs, error) {
	var g ResourceLogs
	err := each(b, func(field, wire int, v []byte, _ uint64) error {
		switch {
		case field == 1 && wire == 2: // Resource
			attrs, err := decodeAttrs(v)
			if err != nil {
				return err
			}
			g.Resource = attrs
		case field == 2 && wire == 2: // ScopeLogs
			return each(v, func(field, wire int, v []byte, _ uint64) error {
				switch {
				case field == 1 && wire == 2: // InstrumentationScope
					return each(v, func(field, wire int, v []byte, _ uint64) error {
						switch {
						case field == 1 && wire == 2:
							g.ScopeName = string(v)
						case field == 2 && wire == 2:
							g.ScopeVersion = string(v)
						}
						return nil
					})
				case field == 2 && wire == 2: // LogRecord
					r, err := decodeRecord(v)
					if err != nil {
						return err
					}
					g.Records = append(g.Records, r)
				}
				return nil
			})
		}
		return nil
	})
	return g, err
}

func decodeRecord(b []byte) (Record, error) {
	var r Record
	err := each(b, func(field, wire int, v []byte, n uint64) error {
		switch {
		case field == 1 && wire == 1:
			r.TimeNano = n
		case field == 2 && wire == 0:
			r.Severity = int32(n)
		case field == 3 && wire == 2:
			r.SeverityText = string(v)
		case field == 5 && wire == 2: // AnyValue body
			return each(v, func(field, wire int, v []byte, _ uint64) error {
				if field == 1 && wire == 2 {
					r.Body = string(v)
				}
				return nil
			})
		case field == 6 && wire == 2:
			a, err := decodeKeyValue(v)
			if err != nil {
				return err
			}
			r.Attrs = append(r.Attrs, a)
		case field == 11 && wire == 1:
			r.ObservedNano = n
		}
		return nil
	})
	return r, err
}

func decodeAttrs(b []byte) ([]Attr, error) {
	var out []Attr
	err := each(b, func(field, wire int, v []byte, _ uint64) error {
		if field != 1 || wire != 2 {
			return nil
		}
		a, err := decodeKeyValue(v)
		if err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

func decodeKeyValue(b []byte) (Attr, error) {
	var a Attr
	err := each(b, func(field, wire int, v []byte, _ uint64) error {
		switch {
		case field == 1 && wire == 2:
			a.Key = string(v)
		case field == 2 && wire == 2: // AnyValue
			return each(v, func(field, wire int, v []byte, n uint64) error {
				switch {
				case field == 1 && wire == 2:
					a.Str = string(v)
				case field == 3 && wire == 0:
					a.Int, a.IsInt = int64(n), true
				}
				return nil
			})
		}
		return nil
	})
	return a, err
}

// each walks one message's fields. A length-delimited field arrives as v;
// a varint or a fixed64 as n.
func each(b []byte, fn func(field, wire int, v []byte, n uint64) error) error {
	for len(b) > 0 {
		tag, k := binary.Uvarint(b)
		if k <= 0 {
			return errors.New("otlp: bad tag")
		}
		b = b[k:]
		field, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			n, k := binary.Uvarint(b)
			if k <= 0 {
				return errors.New("otlp: bad varint")
			}
			b = b[k:]
			if err := fn(field, wire, nil, n); err != nil {
				return err
			}
		case 1:
			if len(b) < 8 {
				return errors.New("otlp: short fixed64")
			}
			n := binary.LittleEndian.Uint64(b[:8])
			b = b[8:]
			if err := fn(field, wire, nil, n); err != nil {
				return err
			}
		case 2:
			l, k := binary.Uvarint(b)
			if k <= 0 || uint64(len(b)-k) < l {
				return errors.New("otlp: bad length")
			}
			v := b[k : k+int(l)]
			b = b[k+int(l):]
			if err := fn(field, wire, v, 0); err != nil {
				return err
			}
		case 5:
			if len(b) < 4 {
				return errors.New("otlp: short fixed32")
			}
			b = b[4:]
		default:
			return fmt.Errorf("otlp: unsupported wire type %d", wire)
		}
	}
	return nil
}
