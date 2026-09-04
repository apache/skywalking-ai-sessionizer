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

// Package mock is the adapter behind Session Data that a scenario writes
// directly, in the model's own vocabulary and under no runtime's dialect.
//
// It collects nothing. It exists so that landed files a scenario produced say
// where they came from, so a receiver lists them under a service of their
// own, and so the glossary test can ask what this dialect calls each name the
// model uses: the same thing, because there is no runtime in between.
package mock

import "github.com/apache/skywalking-ai-sessionizer/pkg/model"

// Name and Version identify the adapter in a landed header; Dialect says the
// records were written in the model's vocabulary.
const (
	Name    = "mock"
	Version = "0.2.0"
	Dialect = "mock/1"
)

// RuntimeName is the service every record is attributed to when a scenario's
// root is pushed and no service name is configured, so a receiver never lists
// invented conversations beside real ones.
const RuntimeName = "Mock Agent"

// Glossary says what the mock calls each name the model uses: the model's own
// word, since the records are written in it. Every name in the vocabulary has
// an entry, which is what the glossary test asks of every dialect.
func Glossary() *model.Glossary {
	var terms []model.Term
	for _, name := range model.Vocabulary() {
		terms = append(terms, model.Term{Unified: name, Native: name, Where: "a landed record, as written",
			Note: "the mock writes the model's vocabulary, so the native word is the model's"})
	}
	return model.NewGlossary(Dialect, terms...)
}
