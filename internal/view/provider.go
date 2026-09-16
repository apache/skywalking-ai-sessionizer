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

package view

import (
	"encoding/json"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// providerBodies takes each call's provider bodies from the fold, and how many
// the session holds from its session node.
//
// The join itself is not made here. It is made once, when a round is parsed, and
// the round carries it: a body is joined to a call by identifiers only its
// manifest holds, and a reader that made the join itself would have to open every
// landed body - the largest files a session holds - to read one line of each. See
// internal/assemble/provider.go for the rules.
//
// A conversation whose rounds predate that carries no bodies here, and the page
// shows the calls without them. Parsing the chain again from the landed files is
// what brings them in.
func (c *Conversation) providerBodies() (int, map[string][]sessionview.ProviderBody) {
	byStep := map[string][]sessionview.ProviderBody{}
	for _, n := range c.View.Nodes {
		if n.Kind != model.KindLLMCall {
			continue
		}
		// A round whose bodies do not read never folds: the reader refuses it. What
		// reaches here is either the shape or nothing.
		carried, err := sessionflow.ProviderBodiesOf(n.Attrs)
		if err != nil {
			continue
		}
		for _, y := range carried {
			byStep[n.ID] = append(byStep[n.ID], sessionview.ProviderBody{Role: y.Role, Ref: y.Ref})
		}
	}
	return providerBodyCount(c.View.Nodes), byStep
}

// providerBodyCount is how many bodies the session holds as of the folded chain,
// joined or not, as the session node states it.
func providerBodyCount(nodes map[string]*sessionflow.Node) int {
	for _, n := range nodes {
		if n.Kind != model.KindSession || len(n.Attrs) == 0 {
			continue
		}
		var a struct {
			Bodies int `json:"provider_bodies_landed"`
		}
		if json.Unmarshal(n.Attrs, &a) == nil {
			return a.Bodies
		}
	}
	return 0
}

// annotateProvider writes each call step's joined bodies onto the step.
func annotateProvider(nodes []sessionview.Node, byStep map[string][]sessionview.ProviderBody) {
	for i := range nodes {
		if bodies := byStep[nodes[i].ID]; len(bodies) > 0 {
			nodes[i].ProviderBodies = bodies
		}
		annotateProvider(nodes[i].Children, byStep)
	}
}
