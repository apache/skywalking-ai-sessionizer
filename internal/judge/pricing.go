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

package judge

import (
	"fmt"
	"io"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// Pricing is the optional price file: what a million tokens of each type
// cost, per model, from a time on.
//
// A model is matched by the exact name the runtime recorded, or by an alias
// written for it. Nothing strips a provider prefix or guesses a family,
// because one short name can be billed differently by two providers.
type Pricing struct {
	Currency string         `yaml:"currency"`
	Models   []PricingEntry `yaml:"models"`

	// byName maps a recorded name to its entries, the latest first.
	byName map[string][]*PricingEntry
}

// PricingEntry is the price of one model from a time on.
type PricingEntry struct {
	Model   string   `yaml:"model"`
	Aliases []string `yaml:"aliases"`
	// From is when the price took effect. An entry without one applies
	// when no dated entry does, and to a call that recorded no time.
	From       string             `yaml:"from"`
	PerMillion sessionjudge.Rates `yaml:"per_million"`

	from time.Time
}

// LoadPricing reads a price file. An empty path is no price file: every call
// is then unpriced, and its tokens still count. The file is a path a person
// gave, checked and opened as one; admitted is the pulled snapshot a held
// lease lets it lie in, or empty.
func LoadPricing(path, admitted string) (*Pricing, error) {
	if path == "" {
		return nil, nil
	}
	b, err := readSupplied(path, admitted, pricingBytes)
	if err != nil {
		return nil, fmt.Errorf("judge: price file: %w", err)
	}
	if len(b) > pricingBytes {
		return nil, fmt.Errorf("judge: price file %s is larger than %d MiB", path, pricingBytes>>20)
	}
	var p Pricing
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("judge: price file %s: %w", path, err)
	}
	if err := p.index(); err != nil {
		return nil, fmt.Errorf("judge: price file %s: %w", path, err)
	}
	return &p, nil
}

func (p *Pricing) index() error {
	if p.Currency == "" {
		return fmt.Errorf("no currency")
	}
	p.byName = map[string][]*PricingEntry{}
	// owner keeps which model a name was claimed for, so an alias cannot
	// mean two models.
	owner := map[string]string{}
	seen := map[string]bool{}
	for i := range p.Models {
		e := &p.Models[i]
		if e.Model == "" {
			return fmt.Errorf("entry %d names no model", i+1)
		}
		if e.From != "" {
			t, err := time.Parse(time.RFC3339, e.From)
			if err != nil {
				return fmt.Errorf("%s: from: %w", e.Model, err)
			}
			e.from = t
		}
		key := e.Model + "\x00" + e.From
		if seen[key] {
			return fmt.Errorf("%s: two entries from %q", e.Model, e.From)
		}
		seen[key] = true
		for _, name := range append([]string{e.Model}, e.Aliases...) {
			if m, ok := owner[name]; ok && m != e.Model {
				return fmt.Errorf("%q names both %s and %s", name, m, e.Model)
			}
			owner[name] = e.Model
			p.byName[name] = append(p.byName[name], e)
		}
	}
	for _, list := range p.byName {
		// Dated entries latest first, then the undated one.
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.from.IsZero() != b.from.IsZero() {
				return !a.from.IsZero()
			}
			return a.from.After(b.from)
		})
	}
	return nil
}

// Lookup returns the price a call of the named model pays at a time, in unix
// milliseconds, 0 when the call recorded none. It returns nil when nothing
// applies: no price file, no entry for the name, or only entries dated after
// the call.
func (p *Pricing) Lookup(model string, at int64) *sessionjudge.Price {
	if p == nil || model == "" {
		return nil
	}
	for _, e := range p.byName[model] {
		if e.from.IsZero() || (at != 0 && !time.UnixMilli(at).Before(e.from)) {
			return &sessionjudge.Price{Model: model, Key: e.Model, From: e.From, Currency: p.Currency, PerMillion: e.PerMillion}
		}
	}
	return nil
}

// PricingOf is the pricing a ledger stage applied: one entry for each rate it
// wrote. A navigation server prices from the run's own ledger this way, so
// every stage of a run sees the costs the ledger counted, even when the price
// file changes or goes while the run goes on. A rate no call used is not in
// the ledger, and no call it reads needs it.
func PricingOf(ledger *sessionjudge.File) (*Pricing, error) {
	if len(ledger.Prices) == 0 {
		return nil, nil
	}
	p := &Pricing{Currency: ledger.Prices[0].Currency}
	for _, pr := range ledger.Prices {
		if pr.Currency != p.Currency {
			return nil, fmt.Errorf("judge: the ledger prices in %s and %s", p.Currency, pr.Currency)
		}
		p.Models = append(p.Models, PricingEntry{Model: pr.Model, From: pr.From, PerMillion: pr.PerMillion})
	}
	if err := p.index(); err != nil {
		return nil, err
	}
	return p, nil
}

// pricingBytes bounds a price file, which lists a few rates per model.
const pricingBytes = 4 << 20

// readSupplied reads a file a person named, up to limit bytes and one past
// it, so the caller can tell a larger one apart and say so. It is checked as
// a supplied path and read through its checked parent: a path in a tree pull
// owns is refused before it is opened, except within admitted, the snapshot a
// held lease covers.
func readSupplied(path, admitted string, limit int) ([]byte, error) {
	f, err := storage.OpenSuppliedRead(path, admitted)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(limit)+1))
}
