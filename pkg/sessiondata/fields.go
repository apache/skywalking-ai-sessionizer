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

package sessiondata

// Fields says what each field of a landed record means, in one line.
//
// A reader looking at a record should not have to read this package to find
// out what it is looking at, so the format carries its own description. Only
// the envelope is here. A field that names something the model names - a
// stream, a call, a run - is described by the adapter's glossary instead, which
// also says what the runtime calls it.
func Fields() map[string]string {
	return map[string]string{
		// header
		"h":       "the file header, written once as the first line",
		"schema":  "the format version of this file",
		"seq":     "the landed sequence of this file, unique across every stream in a session",
		"at":      "when this file was written",
		"kind":    "what the file holds: a stream's records, a run journal, a manifest or a script",
		"adapter": "which adapter produced the file",
		"dialect": "how to read the records: the runtime's vocabulary and version",
		"src":     "the source file the records were read from",

		// record position and identity
		"ord":   "the line this record came from in its source file, counting from one",
		"off":   "the byte the record starts at in its source file",
		"sha":   "a digest of the source bytes, so a record can be checked against its source",
		"bytes": "how many bytes the record is",

		// record content
		"label":      "a short name the runtime gave this record",
		"started_by": "what asked for the work this record belongs to",
		"from":       "who produced the record: the agent, something outside it, or the runtime",
		"flags":      "short facts about the record that do not belong in a field of their own",
		"usage":      "tokens the provider reported for this call",
		"parts":      "the pieces of the record, in the order they were written",
		"dropped":    "what was left out of this record, how big it was, and why",

		// parts
		"k":      "what kind of piece this is: text, reasoning, a call, a result, media or data",
		"text":   "the readable text of this piece",
		"data":   "the piece as bytes, kept whole where it has no readable text",
		"of":     "the call this piece answers",
		"name":   "what was called",
		"failed": "whether the call reported a failure",
		"media":  "the type of the media this piece carries",
		"state":  "whether the content is here, and if not, why not",

		// usage
		"in":          "tokens sent",
		"out":         "tokens returned",
		"cache_read":  "tokens read from the provider's cache",
		"cache_write": "tokens written to the provider's cache",

		// dropped
		"what": "the field that was left out",
		"why":  "why it was left out",
	}
}
