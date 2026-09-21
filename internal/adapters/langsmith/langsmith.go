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

// Package langsmith is the langsmith-ingest adapter: a receiver the LangSmith
// tracing client is pointed at, and the dialect that reads what it sends.
//
// An application built on LangChain or LangGraph already carries this client,
// because langsmith is a dependency of langchain-core. So four environment
// variables are the whole integration, and nothing in the application changes:
//
//	LANGSMITH_TRACING=true
//	LANGSMITH_ENDPOINT=http://<asz>:1985
//	LANGSMITH_API_KEY=<anything this receiver accepts>
//	LANGSMITH_PROJECT=<a name>
//
// This is interoperation, not impersonation: the receiver accepts what that
// client sends, carries none of its code, and claims no affiliation.
//
// # What the client does, measured
//
// The fixtures under tests/apps/langchain hold what one client actually sent.
// Four of its behaviours shape everything here.
//
// It asks GET /info before every send, and takes the batch configuration from
// the answer. Compression is part of that answer: the client compresses only
// when instance_flags.zstd_compression_enabled is set, so a receiver that does
// not advertise it never has to read zstd.
//
// It posts runs that have not finished. Six of eighteen runs in one capture
// arrived with no end time and were completed twelve seconds later by a patch.
// So a run arrives one or more times, and a receiver that expects a finished
// run will miss the work in progress that only this transport carries.
//
// It retries a rejected batch with the same bytes. Delivery is at-least-once
// and deduplication is the receiver's job.
//
// It sends inputs and outputs as their own parts rather than as attributes, so
// a 20,908-byte command and a 192,000-byte result arrive whole.
package langsmith

const (
	// Name is the adapter's name in the configuration.
	Name = "langsmith-ingest"
	// Dialect is whose vocabulary the landed records were read in.
	Dialect = "langsmith/1"
	// Version is the adapter's contract version.
	Version = "0.1.0"
	// RuntimeName is what the landed data is attributed to by default. The
	// wire is LangSmith's; what produced it is LangChain, and an application
	// may say otherwise in the configuration.
	RuntimeName = "LangChain"

	// SourceRuns names the spool files this adapter writes: one per accepted
	// request, holding the body as it arrived.
	SourceRuns = "runs"

	// MaxRequest bounds one request. The client is told 20 MiB in the batch
	// configuration it reads from GET /info; this is the hard stop above it,
	// since nothing makes a client obey what it was told.
	MaxRequest = 64 << 20

	// BatchSizeLimit and BatchSizeLimitBytes are what GET /info tells the
	// client to send in one request. They are its numbers, not ours: the
	// client's own defaults, repeated so a receiver that answers is not
	// silently changing how it batches.
	BatchSizeLimit      = 100
	BatchSizeLimitBytes = 20 << 20
)
