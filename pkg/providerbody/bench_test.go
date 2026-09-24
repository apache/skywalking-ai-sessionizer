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

package providerbody_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
)

// BenchmarkLangChainRequests measures cutting every request body of one
// LangChain conversation, as the langsmith-ingest receiver does. One
// operation is one whole conversation.
//
// LangChain sends the whole history again on every model call, so the bytes
// on the wire grow with the square of the conversation's length, and what the
// cutter costs grows with them. The conversation is generated, seeded, in
// LangChain's serialized message form: a system prompt, then turns of a human
// question, the agent's tool calls, tool results of a heavy-tailed size - 70%
// about 1 KB, 25% about 6 KB, 5% about 40 KB - and an answer. Once the history
// fills the context window it is handled one of three ways, for 199 more agent
// calls, plus the summariser's calls:
//
//	none       the conversation stops when the window is full
//	trim       the oldest messages are dropped to 70% of the window
//	summarise  a summariser call reads the whole history, and the next request is
//	           a summary message plus the last four messages
//
// Both keep a fixed number of the latest messages whatever their size: trim two,
// summarise four. At 8K one 40 KB tool result among them leaves the request
// above 70%, and above the window. The rows below include that.
//
// Reported beside time and allocations: MB/s of wire bytes, landed-% (the
// landed records against the wire bytes) and calls (model calls in the
// conversation). landed-% and calls are deterministic, so any change in them
// is a change in behaviour, not noise.
//
// The 1M window runs only with ASZ_BENCH_1M=1: one operation takes 15 to 20
// seconds.
//
// Not run by make test or CI. Run it with:
//
//	go test -run '^$' -bench LangChainRequests -benchmem -count 10 ./pkg/providerbody/
//
// Compare a change with main by running both on one machine, back to back,
// and reading them with benchstat.
//
// Reference, measured 2026-09-23 with the command above, median of ten:
// Apple M3 Max, 16 cores, 128 GB, macOS 27.0 (Darwin 27.0.0), Go 1.27.1
// darwin/arm64, on power, nothing else heavy running. Timings from another
// machine are not comparable with these; landed-% and calls are.
//
//	window  strategy   calls  time/op  MiB/s  landed-%      B/op  allocs/op
//	8K      none           5  1.29 ms   19.2     50.53  15.3 MiB        811
//	8K      trim         204  53.4 ms   84.4     24.16   639 MiB      29.8k
//	8K      summarise    249  74.2 ms   81.1     43.26   794 MiB      36.6k
//	32K     none          48  27.9 ms    111     7.977   154 MiB      7.49k
//	32K     trim         247   173 ms    134     6.219   811 MiB      35.2k
//	32K     summarise    224   121 ms    126     11.96   726 MiB      30.9k
//	128K    none         122   233 ms    148     2.365   440 MiB      17.6k
//	128K    trim         321   783 ms    149     2.205  1.19 GiB      48.2k
//	128K    summarise    342   515 ms    146     4.376  1.18 GiB      49.5k
//	200K    none         282   725 ms    146     2.061  1.05 GiB      41.9k
//	200K    trim         481   1.57 s    151     1.982  1.91 GiB      75.2k
//	200K    summarise    361   831 ms    149     2.749  1.33 GiB      53.4k
//	1M      none        1104   14.9 s    152     0.926  10.9 GiB       221k
//	1M      trim        1303   19.1 s    152     0.928  13.7 GiB       270k
//	1M      summarise   1337   15.8 s    153     1.128  11.9 GiB       262k
//
// benchstat gives each time a 95% confidence range around the median. It was
// within 3%, except 8K/none at 7%, 32K/none at 5%, and 8K/summarise and
// 128K/none at 4%. Treat a smaller difference as noise. Cutting runs at about
// 150 MiB/s once the window is 128K or more. It allocates far
// more than it cuts: about 5 times the wire bytes at 1M, 8 to 10 times at 128K
// and 200K, 35 times at 32K and 140 times at 8K (trim). The allocation is what
// to look at first if the time ever matters.
func BenchmarkLangChainRequests(b *testing.B) {
	windows := []struct {
		name   string
		tokens int
	}{{"8K", 8_000}, {"32K", 32_000}, {"128K", 128_000}, {"200K", 200_000}, {"1M", 1_000_000}}
	for _, w := range windows {
		for _, strategy := range []string{"none", "trim", "summarise"} {
			b.Run(w.name+"/"+strategy, func(b *testing.B) {
				if w.tokens >= 1_000_000 && os.Getenv("ASZ_BENCH_1M") != "1" {
					b.Skip("the 1M window runs only with ASZ_BENCH_1M=1")
				}
				bodies := conversation(w.tokens, strategy)
				var wire int64
				for _, body := range bodies {
					wire += int64(len(body))
				}
				b.SetBytes(wire)
				b.ReportAllocs()
				var landed int64
				for b.Loop() {
					held := providerbody.NewSession()
					landed = 0
					for i, body := range bodies {
						rec, err := held.Encode(providerbody.Body{ID: fmt.Sprintf("r%d:request", i),
							Role: providerbody.RoleRequest, Src: "bench", Keys: providerbody.Keys{Session: "s"}, Bytes: body})
						if err != nil {
							b.Fatal(err)
						}
						landed += providerbody.RecordBytes(rec)
					}
				}
				b.ReportMetric(100*float64(landed)/float64(wire), "landed-%")
				b.ReportMetric(float64(len(bodies)), "calls")
			})
		}
	}
}

// The generator runs inside a benchmark, never at package level: make test
// compiles this file and must not pay for conversations it does not use.

type benchMessage struct {
	Type, ID, Content, ToolCallID, ToolCall string
}

var benchWords = strings.Fields("service latency checkout payment pool connection timeout p99 error rate " +
	"inventory database query slow retry budget deploy index migration ledger account order " +
	"health ready replicas pods node cluster region upstream downstream request response")

func benchText(r *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(benchWords[r.Intn(len(benchWords))])
		b.WriteByte(' ')
	}
	return b.String()[:n]
}

func benchToolSize(r *rand.Rand) int {
	switch x := r.Float64(); {
	case x < 0.70:
		return 800 + r.Intn(400)
	case x < 0.95:
		return 5000 + r.Intn(2000)
	default:
		return 36000 + r.Intn(8000)
	}
}

func benchBody(msgs []benchMessage) []byte {
	list := make([]any, len(msgs))
	for i, m := range msgs {
		kind := map[string]string{"system": "SystemMessage", "human": "HumanMessage", "ai": "AIMessage", "tool": "ToolMessage"}[m.Type]
		kw := map[string]any{"content": m.Content, "type": m.Type}
		if m.ID != "" {
			kw["id"] = m.ID
		}
		if m.ToolCallID != "" {
			kw["tool_call_id"] = m.ToolCallID
		}
		if m.ToolCall != "" {
			kw["tool_calls"] = []any{map[string]any{"name": "service_health", "args": map[string]any{"service": "checkout"},
				"id": m.ToolCall, "type": "tool_call"}}
		}
		list[i] = map[string]any{"lc": 1, "type": "constructor", "id": []string{"langchain", "schema", "messages", kind}, "kwargs": kw}
	}
	b, _ := json.Marshal(map[string]any{"messages": [][]any{list}})
	return b
}

func benchSize(msgs []benchMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + 120
	}
	return n
}

// conversation returns the request body of every model call of one
// conversation for a context window of tokens, about four characters each.
func conversation(tokens int, strategy string) [][]byte {
	const extra = 200
	r := rand.New(rand.NewSource(int64(tokens)*7 + int64(len(strategy))))
	limit := tokens * 4
	next := 0
	id := func(p string) string { next++; return fmt.Sprintf("%s-%08d", p, next) }
	sys := benchMessage{Type: "system", Content: benchText(r, 1500)} // no id, as LangChain sends it
	history := []benchMessage{{Type: "human", ID: id("h"), Content: benchText(r, 150)}}
	var bodies [][]byte
	full, after := false, 0
	for n := 0; ; n++ {
		bodies = append(bodies, benchBody(append([]benchMessage{sys}, history...)))
		if n%4 == 3 {
			history = append(history, benchMessage{Type: "ai", ID: id("run"), Content: benchText(r, 600)},
				benchMessage{Type: "human", ID: id("h"), Content: benchText(r, 150)})
		} else {
			tc := id("call")
			history = append(history, benchMessage{Type: "ai", ID: id("run"), Content: benchText(r, 200), ToolCall: tc},
				benchMessage{Type: "tool", ID: id("t"), Content: benchText(r, benchToolSize(r)), ToolCallID: tc})
		}
		if benchSize(history) > limit {
			full = true
			switch strategy {
			case "none":
				return bodies
			case "trim":
				for benchSize(history) > limit*7/10 && len(history) > 2 {
					history = history[1:]
				}
			case "summarise":
				var all strings.Builder
				for _, m := range history {
					all.WriteString(m.Type + ": " + m.Content + "\n")
				}
				summary := benchText(r, 2000)
				bodies = append(bodies, benchBody([]benchMessage{{Type: "human", ID: id("h"),
					Content: "Summarise this conversation:\n" + all.String()}}))
				keep := history[len(history)-4:]
				history = append([]benchMessage{{Type: "human", ID: id("h"),
					Content: "Here is a summary of the conversation to date:\n\n" + summary}}, keep...)
			}
		}
		// after counts the call that first fills the window, so extra-1 agent
		// calls follow it. Summariser calls are not counted.
		if full {
			after++
			if after >= extra {
				return bodies
			}
		}
	}
}
