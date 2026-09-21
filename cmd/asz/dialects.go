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

package main

import (
	"os"
	"sort"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// glossaries is every vocabulary this command can render a name in.
//
// A dialect is not an adapter: it says how records were read, not how they
// were acquired, so a pull reader and a push receiver for one runtime share
// one entry here.
func glossaries() map[string]*model.Glossary {
	return map[string]*model.Glossary{
		claudecode.Dialect: claudecode.Glossary(),
		langsmith.Dialect:  langsmith.Glossary(),
	}
}

// dialectNames lists what a reader can ask for.
func dialectNames() []string {
	var out []string
	for name := range glossaries() {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// glossaryFor returns a dialect's vocabulary, and whether it is one this
// command knows.
func glossaryFor(dialect string) (*model.Glossary, bool) {
	g, ok := glossaries()[dialect]
	return g, ok
}

// dialectOf reads which vocabulary a session's records were read in.
//
// It comes from the data rather than from the configuration, because a root
// can hold sessions from more than one runtime and a reader asking for "the
// runtime's words" means the ones belonging to what they are looking at. A
// session whose dialect is not one this command knows renders in the model's
// own names, which is always correct if less familiar.
//
// A session holds more than conversation records. The plugin's file changes
// land beside them in a dialect of their own, and whichever file came first
// used to decide the whole session - so a session whose changes landed
// before its transcript rendered in the changes vocabulary and none of the
// runtime's words were available at all. The conversation's own records
// decide it, and the rest only when there are none.
func dialectOf(zone *storage.Zone, session string) string {
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		return ""
	}
	var fallback string
	for _, f := range files {
		kind, dialect := headerOf(f.Path)
		if dialect == "" {
			continue
		}
		if kind == sessiondata.KindTranscript {
			return dialect
		}
		if fallback == "" {
			fallback = dialect
		}
	}
	return fallback
}

// headerOf reads one landed file's kind and dialect, and nothing else.
func headerOf(path string) (sessiondata.Kind, string) {
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	reader, err := sessiondata.NewReader(file)
	if err != nil {
		return "", ""
	}
	return reader.Header().Kind, reader.Header().Dialect
}
