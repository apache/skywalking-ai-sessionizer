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

package sessionview

import (
	"encoding/json"

	"gopkg.in/yaml.v3"
)

// MarshalYAML renders a document as YAML with the same keys in the same
// order as its JSON. The document is defined as JSON; YAML is a rendering
// for a person reading it in a terminal or diffing two of them, never a
// second format, so it is produced from the JSON rather than from the
// types, and a reader of either sees one document.
func MarshalYAML(doc *Conversation) ([]byte, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// JSON is YAML, so decoding it into a node keeps every key in order.
	// Decoded from JSON the nodes carry JSON's style; cleared, the encoder
	// writes blocks and plain scalars.
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	unflow(&node)
	return yaml.Marshal(&node)
}

// unflow drops the style JSON gave every node, so the encoder writes block
// mappings and plain scalars, quoting only what would otherwise be misread.
func unflow(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		unflow(c)
	}
}
