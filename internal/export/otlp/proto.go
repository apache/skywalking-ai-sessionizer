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

// Package otlp pushes landed files and rounds to an OpenTelemetry logs
// receiver over OTLP/HTTP, one log record per file line.
//
// The protobuf encoding is written here by hand. The logs request is a small
// message, the wire format is a handful of rules, and the alternative is the
// protobuf and OpenTelemetry modules, which would multiply the dependencies of
// a project that has one. Field numbers follow opentelemetry/proto v1.
package otlp

import "encoding/binary"

// Attr is one string or integer attribute. Only these two kinds are sent.
type Attr struct {
	Key string
	Str string
	Int int64
	// IsInt selects the integer value; the string value is sent otherwise.
	IsInt bool
}

// Record is one OTLP log record.
type Record struct {
	TimeNano     uint64
	ObservedNano uint64
	Severity     int32
	SeverityText string
	Body         string
	Attrs        []Attr
}

// ResourceLogs is the records of one resource, under one scope.
type ResourceLogs struct {
	Resource     []Attr
	ScopeName    string
	ScopeVersion string
	Records      []Record
}

// Encode returns an ExportLogsServiceRequest as protobuf bytes.
func Encode(groups []ResourceLogs) []byte {
	var b []byte
	for _, g := range groups {
		b = appendMessage(b, 1, encodeResourceLogs(g))
	}
	return b
}

func encodeResourceLogs(g ResourceLogs) []byte {
	var b []byte
	b = appendMessage(b, 1, encodeAttrs(g.Resource)) // Resource{attributes=1}
	var scope []byte
	scope = appendMessage(scope, 1, encodeScope(g.ScopeName, g.ScopeVersion))
	for _, r := range g.Records {
		scope = appendMessage(scope, 2, encodeRecord(r))
	}
	b = appendMessage(b, 2, scope) // scope_logs=2
	return b
}

func encodeScope(name, version string) []byte {
	var b []byte
	b = appendString(b, 1, name)
	b = appendString(b, 2, version)
	return b
}

func encodeAttrs(attrs []Attr) []byte {
	var b []byte
	for _, a := range attrs {
		b = appendMessage(b, 1, encodeKeyValue(a))
	}
	return b
}

func encodeKeyValue(a Attr) []byte {
	var b []byte
	b = appendString(b, 1, a.Key)
	var v []byte
	if a.IsInt {
		v = appendVarint(appendTag(v, 3, 0), uint64(a.Int)) // int_value=3
	} else {
		v = appendString(v, 1, a.Str) // string_value=1
	}
	return appendMessage(b, 2, v)
}

func encodeRecord(r Record) []byte {
	var b []byte
	b = appendFixed64(b, 1, r.TimeNano)
	if r.Severity != 0 {
		b = appendVarint(appendTag(b, 2, 0), uint64(r.Severity))
	}
	b = appendString(b, 3, r.SeverityText)
	var body []byte
	body = appendString(body, 1, r.Body)
	b = appendMessage(b, 5, body)
	for _, a := range r.Attrs {
		b = appendMessage(b, 6, encodeKeyValue(a))
	}
	b = appendFixed64(b, 11, r.ObservedNano)
	return b
}

// Wire-format primitives. A tag is the field number shifted left by three
// with the wire type in the low bits: 0 varint, 1 fixed64, 2 length-delimited.

func appendTag(b []byte, field int, wire int) []byte {
	return appendVarint(b, uint64(field<<3|wire))
}

func appendVarint(b []byte, v uint64) []byte {
	return binary.AppendUvarint(b, v)
}

func appendFixed64(b []byte, field int, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = appendTag(b, field, 1)
	return binary.LittleEndian.AppendUint64(b, v)
}

func appendString(b []byte, field int, s string) []byte {
	if s == "" {
		return b
	}
	b = appendTag(b, field, 2)
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendMessage(b []byte, field int, m []byte) []byte {
	b = appendTag(b, field, 2)
	b = appendVarint(b, uint64(len(m)))
	return append(b, m...)
}
