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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

const measureUsage = `Usage: asz measure [-config FILE] [-pricing FILE] [-json] CONVERSATION

Prints what a conversation's model calls consumed: tokens by type, money where
the price file prices a call, per talk with the child agents it started. It
computes from the conversation's rounds and landed files and calls no model.
`

// loadConfig reads -config, or ./asz.yaml when present, as every command does.
func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		if _, err := os.Stat(defaultConfig); err == nil {
			path = defaultConfig
		}
	}
	return config.Load(path)
}

func cmdMeasure(args []string) int {
	fs := flag.NewFlagSet("measure", flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file")
	pricingPath := fs.String("pricing", "", "price file; overrides judge.pricing")
	asJSON := fs.Bool("json", false, "print the ledger as JSON")
	fs.Usage = func() { fmt.Fprint(os.Stderr, measureUsage) }
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)
	if err := checkIdentity("conversation", id, false); err != nil {
		return fail(err)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return fail(err)
	}
	root, err := cfg.ResolvedRoot()
	if err != nil {
		return fail(err)
	}
	if _, err := admitRoot("measure", cfg, root); err != nil {
		return fail(err)
	}
	defer releaseAdmission()
	if *pricingPath == "" {
		*pricingPath = cfg.Judge.Pricing
	}
	led, err := measure(cfg, id, *pricingPath)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		out, err := json.MarshalIndent(ledgerDocument(led), "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(out))
		return 0
	}
	printLedger(id, led)
	return 0
}

// measure computes a conversation's ledger.
func measure(cfg *config.Config, id, pricingPath string) (*judge.Ledger, error) {
	root, err := cfg.ResolvedRoot()
	if err != nil {
		return nil, err
	}
	pricing, err := judge.LoadPricing(pricingPath, admittedRoot())
	if err != nil {
		return nil, err
	}
	zone := storage.NewZone(root)
	srv, snap, err := viewServer(zone, nil)
	if err != nil {
		return nil, err
	}
	c, err := srv.Load(id)
	if err != nil {
		return nil, err
	}
	doc, err := c.Build()
	if err != nil {
		return nil, err
	}
	if snap != nil {
		// A pulled snapshot's records are read checked: one that cannot be
		// read stops the ledger rather than leaving its usage out.
		return judge.ComputeChecked(doc, dialectOf(zone, c.Session), pricing, c.RecordsChecked)
	}
	return judge.Compute(doc, dialectOf(zone, c.Session), pricing, c.Records), nil
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "asz:", err)
	return 1
}

// ledgerDocument is what -json prints: the frames a ledger stage writes.
func ledgerDocument(l *judge.Ledger) map[string]any {
	talks := []sessionjudge.Cost{}
	for _, t := range l.Main {
		if c, ok := l.Inclusive(t); ok {
			talks = append(talks, c)
		}
	}
	return map[string]any{
		"dialect": l.Dialect, "currency": l.Currency, "total": l.Total,
		"owners": l.Owners, "main_talks": talks, "unattributed": l.Unattributed, "prices": l.Prices,
	}
}

func printLedger(id string, l *judge.Ledger) {
	t := l.Total
	fmt.Printf("conversation %s  (%s)\n", id, l.Dialect)
	fmt.Printf("  calls        %d, %d with unknown usage, %d that never finished, their output unknown\n", t.Calls, t.Unknown, t.OutputUnknown)
	fmt.Printf("  tokens       in %d  out %d  cache read %d  cache write %d\n",
		t.Usage.Input, t.Usage.Output, t.Usage.CacheRead, t.Usage.CacheWrite)
	switch {
	case t.Money != nil:
		fmt.Printf("  money        %.2f %s, %d calls with usage unpriced\n", *t.Money, t.Currency, t.Unpriced)
	default:
		fmt.Printf("  money        unavailable: no call was priced\n")
	}
	if l.Unattributed.Calls > 0 {
		fmt.Printf("  unattributed %d calls of child streams no single talk started\n", l.Unattributed.Calls)
	}
	type row struct {
		id string
		c  sessionjudge.Cost
	}
	var rows []row
	for _, tid := range l.Main {
		if c, ok := l.Inclusive(tid); ok {
			rows = append(rows, row{tid, c})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return weight(rows[i].c) > weight(rows[j].c) })
	fmt.Printf("\n  %d main talks; the costliest, with the child agents each started:\n", len(rows))
	for i, r := range rows {
		if i == 10 {
			break
		}
		label := ""
		if n := l.Talk(r.id); n != nil {
			label = trunc(strings.Join(strings.Fields(n.Label), " "), 60)
		}
		money := "-"
		if r.c.Money != nil {
			money = fmt.Sprintf("%.2f", *r.c.Money)
		}
		fmt.Printf("  %8s  %4d calls (%d in children)  %s\n", money, r.c.Calls, r.c.Children, label)
	}
}

// weight orders talks by what they cost when priced, else by their tokens.
func weight(c sessionjudge.Cost) float64 {
	if c.Money != nil {
		return *c.Money
	}
	u := c.Usage
	return float64(u.Input + u.Output + u.CacheRead + u.CacheWrite)
}
