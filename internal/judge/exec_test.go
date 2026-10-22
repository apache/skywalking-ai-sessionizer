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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// The signature matches the worked example of Signature Version 4 that AWS
// publishes: a GET to IAM on 2015-08-30 at 12:36 UTC.
func TestSignatureMatchesAWSWorkedExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	c := awsCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	sign(req, nil, c, "us-east-1", "iam", "iam.amazonaws.com", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization\n got %s\nwant %s", got, want)
	}
}

// fakeAPI answers an API evaluator: the first call is answered with a read of
// the card, the second with the submitted submitted. It keeps what each request
// sent.
func fakeAPI(t *testing.T, first, second string) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Path+" "+r.Header.Get("Authorization")+r.Header.Get("X-Api-Key")+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			io.WriteString(w, first)
			return
		}
		io.WriteString(w, second)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

const submitted = `{"targets":[],"inputs":[]}`

func runAPI(t *testing.T, conf config.Evaluator) (Result, error) {
	t.Helper()
	ex, err := NewExecutor("e", conf)
	if err != nil {
		t.Fatal(err)
	}
	l := Compute(doc(), "claude-code/1", nil, nil)
	return ex.Run(context.Background(), Task{Stage: "targets", Prompt: "judge", Schema: json.RawMessage(`{"type":"object"}`),
		Dir: t.TempDir(), Nav: NewNav(doc(), l, nil)})
}

// Each API reads through the navigation tools and answers through submit;
// the tool's submitted is sent back, and the usage of every call is added up.
func TestAnAPIEvaluatorReadsThenSubmits(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		srv, bodies := fakeAPI(t,
			`{"content":[{"type":"tool_use","id":"u1","name":"card","input":{}}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":5}}`,
			`{"content":[{"type":"tool_use","id":"u2","name":"submit","input":`+submitted+`}],"usage":{"input_tokens":3,"output_tokens":4,"cache_creation_input_tokens":7}}`)
		t.Setenv("TEST_KEY", "k")
		res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m", APIKeyEnv: "TEST_KEY"})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Output) != submitted || res.Usage.Input != 13 || res.Usage.CacheRead != 5 || res.Usage.CacheWrite != 7 {
			t.Fatalf("result %s %+v", res.Output, res.Usage)
		}
		if !strings.HasPrefix((*bodies)[0], "/v1/messages k ") || !strings.Contains((*bodies)[1], `"tool_use_id":"u1"`) ||
			!strings.Contains((*bodies)[1], `\"scope\":\"conversation\"`) {
			t.Fatalf("second request did not carry the card: %s", (*bodies)[1])
		}
	})
	t.Run("openai", func(t *testing.T) {
		srv, bodies := fakeAPI(t,
			`{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"card","arguments":"{}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":60}}}`,
			`{"choices":[{"message":{"tool_calls":[{"id":"c2","function":{"name":"submit","arguments":`+mustQuote(submitted)+`}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
		res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "openai", Endpoint: srv.URL + "/v1", Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		// OpenAI counts the cached part inside its prompt tokens.
		if string(res.Output) != submitted || res.Usage.Input != 50 || res.Usage.CacheRead != 60 {
			t.Fatalf("result %s %+v", res.Output, res.Usage)
		}
		if !strings.HasPrefix((*bodies)[0], "/v1/chat/completions") || !strings.Contains((*bodies)[1], `"tool_call_id":"c1"`) {
			t.Fatalf("second request %s", (*bodies)[1])
		}
	})
	t.Run("bedrock", func(t *testing.T) {
		srv, bodies := fakeAPI(t,
			`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"b1","name":"card","input":{}}}]}},"usage":{"inputTokens":9,"outputTokens":1}}`,
			`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"b2","name":"submit","input":`+submitted+`}}]}},"usage":{"inputTokens":4,"outputTokens":2}}`)
		t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
		t.Setenv("AWS_SESSION_TOKEN", "")
		res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "bedrock", Endpoint: srv.URL, Model: "us.deepseek.r1-v1:0"})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Output) != submitted || res.Usage.Input != 13 {
			t.Fatalf("result %s %+v", res.Output, res.Usage)
		}
		first := (*bodies)[0]
		if !strings.HasPrefix(first, "/model/us.deepseek.r1-v1:0/converse AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
			t.Fatalf("first request %s", first[:120])
		}
		if !strings.Contains((*bodies)[1], `"toolUseId":"b1"`) {
			t.Fatalf("second request %s", (*bodies)[1])
		}
	})
	t.Run("bedrock with an API key", func(t *testing.T) {
		answer := `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"b2","name":"submit","input":` + submitted + `}}]}},"usage":{"inputTokens":4,"outputTokens":2}}`
		srv, bodies := fakeAPI(t, answer, answer)
		// No AWS credentials: the key alone authorizes the request.
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		t.Setenv("BEDROCK_KEY", "ABSKexample")
		res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "bedrock", Endpoint: srv.URL, Model: "deepseek.v3.2", APIKeyEnv: "BEDROCK_KEY"})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Output) != submitted || !strings.HasPrefix((*bodies)[0], "/model/deepseek.v3.2/converse Bearer ABSKexample") {
			t.Fatalf("result %s, request %s", res.Output, (*bodies)[0][:80])
		}
	})
}

// A model that never submits ends the stage with an error after its turns,
// and a reply in text is asked once more for the tool.
func TestAnAPIEvaluatorThatNeverSubmitsFails(t *testing.T) {
	srv, bodies := fakeAPI(t, `{"content":[{"type":"text","text":"done"}],"usage":{}}`, `{"content":[{"type":"text","text":"done"}],"usage":{}}`)
	_, err := runAPI(t, config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m", MaxTurns: 3})
	if err == nil || len(*bodies) != 3 {
		t.Fatalf("err %v after %d calls", err, len(*bodies))
	}
	if !strings.Contains((*bodies)[1], "Call the submit tool") {
		t.Fatalf("the model was not asked for the tool: %s", (*bodies)[1])
	}
}

// Old tool results are taken out of context once the newest pass the bound.
func TestOldToolResultsAreMasked(t *testing.T) {
	big := strings.Repeat("x", keptResultBytes/2+1)
	turns := []turn{{role: "user", results: []toolResult{{ID: "1", Text: big}}},
		{role: "user", results: []toolResult{{ID: "2", Text: big}}},
		{role: "user", results: []toolResult{{ID: "3", Text: "small"}}}}
	maskOld(turns)
	if !turns[0].results[0].masked || turns[1].results[0].masked || turns[2].results[0].masked {
		t.Fatalf("masked %v %v %v", turns[0].results[0].masked, turns[1].results[0].masked, turns[2].results[0].masked)
	}
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// An submitted in text is found at its end, past any prose before it.
func TestTheLastObjectOfAnAnswerIsTaken(t *testing.T) {
	got, err := lastObject("I read it. {\"a\":1} and then {\"targets\":[{\"id\":\"t1\"}],\"inputs\":[]}\n")
	if err != nil || string(got) != `{"targets":[{"id":"t1"}],"inputs":[]}` {
		t.Fatalf("got %s %v", got, err)
	}
	if _, err := lastObject("no submitted"); err == nil {
		t.Fatal("text with no object gave one")
	}
}

// A Bedrock request for a model id with a colon is signed as AWS's own signer
// signs it: the vector comes from botocore's SigV4Auth, for this request at
// 2026-10-20 12:00:00 UTC.
func TestBedrockSignatureMatchesBotocore(t *testing.T) {
	path := "/model/" + awsEscape("us.deepseek.r1-v1:0") + "/converse"
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com"+path, strings.NewReader(`{"messages":[]}`))
	req.URL.RawPath = path
	req.Header.Set("Content-Type", "application/json")
	c := awsCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", SessionToken: "TOKEN"}
	sign(req, []byte(`{"messages":[]}`), c, "us-east-1", "bedrock", "bedrock-runtime.us-east-1.amazonaws.com",
		time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261020/us-east-1/bedrock/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date;x-amz-security-token, Signature=6995636be8c24a8b088b00a6906dc088f9d87b39dfcd255c5b29501139f0f011"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization\n got %s\nwant %s", got, want)
	}
	if path != "/model/us.deepseek.r1-v1%3A0/converse" {
		t.Fatalf("path %s", path)
	}
}

// An API that reports no usage leaves every request's usage unknown:
// counted apart, never taken for zero.
func TestUsageNobodyReportedIsUnknown(t *testing.T) {
	srv, _ := fakeAPI(t,
		`{"content":[{"type":"tool_use","id":"u1","name":"card","input":{}}]}`,
		`{"content":[{"type":"tool_use","id":"u2","name":"submit","input":`+submitted+`}]}`)
	res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if res.UnknownRequests != 2 || res.Usage == nil || res.Usage.Input+res.Usage.Output != 0 {
		t.Fatalf("usage %+v, %d unknown", res.Usage, res.UnknownRequests)
	}
}

// A model given as an ARN, slashes and colons in it, is signed as botocore
// signs it.
func TestBedrockSignatureOfAnARN(t *testing.T) {
	model := "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.deepseek.r1-v1:0"
	path := "/model/" + awsEscape(model) + "/converse"
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com"+path, strings.NewReader(`{"messages":[]}`))
	req.URL.RawPath = path
	req.Header.Set("Content-Type", "application/json")
	c := awsCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", SessionToken: "TOKEN"}
	sign(req, []byte(`{"messages":[]}`), c, "us-east-1", "bedrock", "bedrock-runtime.us-east-1.amazonaws.com",
		time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=9cfd39a27bf29e37ff4e3ae21106164673fb7412700b7ac8f8121c78fb1840b0") {
		t.Fatalf("authorization %s", got)
	}
}

// One response with no usage is counted apart, and the others' usage is
// kept, so what is known is not lost and what is not is never estimated.
func TestUsageIsUnknownWhenOneTurnSaysNone(t *testing.T) {
	srv, _ := fakeAPI(t,
		`{"content":[{"type":"tool_use","id":"u1","name":"card","input":{}}],"usage":{"input_tokens":10,"output_tokens":2}}`,
		`{"content":[{"type":"tool_use","id":"u2","name":"submit","input":`+submitted+`}]}`)
	res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m"})
	if err != nil || res.Usage == nil || res.Usage.Input != 10 || res.UnknownRequests != 1 {
		t.Fatalf("usage %+v, %d unknown, %v", res.Usage, res.UnknownRequests, err)
	}
}

// A request whose answer is lost on the way fails the call, and keeps what
// the requests before it consumed, the lost one counted apart.
func TestARequestLostOnTheWayIsCountedApart(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"content":[{"type":"tool_use","id":"u1","name":"card","input":{}}],"usage":{"input_tokens":10,"output_tokens":2}}`)
			return
		}
		// The connection drops before any answer, as Bedrock's did.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m"})
	if err == nil || res.Usage == nil || res.Usage.Input != 10 || res.UnknownRequests != 1 || len(res.Requests) != 1 {
		t.Fatalf("usage %+v, %d unknown, %d requests, %v", res.Usage, res.UnknownRequests, len(res.Requests), err)
	}
}

// Kiro's agent goes in the stage's directory and never makes a directory
// above it, so a session retired while the stage starts is not made again.
func TestKiroMakesNothingAboveItsStage(t *testing.T) {
	// The stage's directory went with its retired session after the
	// prompt was written.
	gone := filepath.Join(t.TempDir(), "session", "judgments", "j000001", "work", "2-targets")
	if _, err := makeKiroAgentDir(gone); err == nil {
		t.Fatal("the agent's directory was made in a stage that is gone")
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(gone))); !os.IsNotExist(err) {
		t.Fatalf("a directory above the stage was made: %v", err)
	}
	// In a stage that is there, it is made.
	if dir, err := makeKiroAgentDir(t.TempDir()); err != nil || filepath.Base(dir) != "agents" {
		t.Fatalf("%s %v", dir, err)
	}
}

// The configured arguments come before the argument that carries the prompt,
// codex's "-" and kiro-cli's prompt, or a CLI reads them as prompt text and
// a setting such as the reasoning effort is lost.
func TestConfiguredArgumentsComeBeforeThePrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "cli")
	record := filepath.Join(dir, "args.txt")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > "+record+"\nexit 1\n"), 0o700)
	for _, program := range []string{"codex", "kiro-cli"} {
		ex, err := NewExecutor(program, config.Evaluator{Kind: "cli", Program: program, Command: script,
			Args: []string{"-c", "model_reasoning_effort=high"}})
		if err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(dir, program)
		_ = os.Mkdir(work, 0o700)
		_, _ = ex.Run(context.Background(), Task{Stage: "targets", Prompt: "judge it", Schema: json.RawMessage(`{}`), Dir: work, MCP: []string{"asz", "judge", "mcp"}})
		b, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		effort := slices.Index(args, "model_reasoning_effort=high")
		if effort < 0 || effort >= len(args)-1 {
			t.Fatalf("%s: the configured arguments are not before the prompt: %q", program, args)
		}
		if program == "codex" && args[len(args)-1] != "-" {
			t.Fatalf("codex does not end with -: %q", args)
		}
	}
}

// claude counts only its main model in its usage field, and runs a small
// model for its own chores beside it. Every model is counted, the one that
// cost the most is named, and the cost claude reports is kept.
func TestClaudeCountsEveryModel(t *testing.T) {
	out := []byte(`{"type":"result","is_error":false,"structured_output":{"targets":[]},
	  "usage":{"input_tokens":6,"output_tokens":820,"cache_read_input_tokens":28207,"cache_creation_input_tokens":15651},
	  "modelUsage":{"claude-haiku-5-5":{"inputTokens":3179,"outputTokens":362,"costUSD":0.0005},
	                "claude-opus-5-5":{"inputTokens":6,"outputTokens":820,"cacheReadInputTokens":28207,"cacheCreationInputTokens":15651,"costUSD":0.1473}},
	  "total_cost_usd":0.1478}`)
	res, err := claudeResult(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "claude-opus-5-5" || *res.Usage != (sessiondata.Usage{Input: 3185, Output: 1182, CacheRead: 28207, CacheWrite: 15651}) ||
		len(res.ByModel) != 2 || *res.CostUSD != 0.1478 {
		t.Fatalf("result %+v usage %+v", res, res.Usage)
	}
}

// Once an API evaluator's reading budget is spent, submitting is the only
// tool it is offered.
func TestAnAPIEvaluatorOverBudgetCanOnlySubmit(t *testing.T) {
	srv, bodies := fakeAPI(t,
		`{"content":[{"type":"tool_use","id":"u1","name":"card","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"content":[{"type":"tool_use","id":"u2","name":"submit","input":`+submitted+`}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	ex, err := NewExecutor("e", config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	nav.ResetBudget(1)
	if _, err := ex.Run(context.Background(), Task{Stage: "targets", Prompt: "judge", Schema: json.RawMessage(`{"type":"object"}`),
		Dir: t.TempDir(), Nav: nav}); err != nil {
		t.Fatal(err)
	}
	tools := func(body string) []string {
		var req struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.Unmarshal([]byte(body[strings.Index(body, "{"):]), &req)
		var out []string
		for _, tl := range req.Tools {
			out = append(out, tl.Name)
		}
		return out
	}
	if first, second := tools((*bodies)[0]), tools((*bodies)[1]); len(first) < 2 || !slices.Equal(second, []string{submitTool}) {
		t.Fatalf("tools offered %v, then %v", first, second)
	}
}

// claude's stream says each request's input side when it starts and its
// output when it ends; each request is counted exactly once, as it ends,
// and the partial text events are not kept.
func TestClaudeRequestsAreCountedAsTheyEnd(t *testing.T) {
	var got []Request
	w := &lineWriter{line: claudeRequests(func(r Request) { got = append(got, r) }, func() {})}
	stream := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":9,"cache_creation_input_tokens":7625,"cache_read_input_tokens":0,"output_tokens":3}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"text":"hel"}}}`,
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":9,"output_tokens":3}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","usage":{"input_tokens":9,"cache_creation_input_tokens":7625,"output_tokens":104}}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m2","model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_read_input_tokens":7634,"output_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":40}}}`,
		`{"type":"result","structured_output":{"ok":true}}`,
	}
	// Written in pieces that cut lines apart, as a pipe does.
	all := strings.Join(stream, "\n") + "\n"
	for i := 0; i < len(all); i += 37 {
		end := min(i+37, len(all))
		_, _ = w.Write([]byte(all[i:end]))
	}
	want := []Request{
		{Model: "claude-opus-5-5", Usage: sessiondata.Usage{Input: 9, CacheWrite: 7625, Output: 104}},
		{Model: "claude-opus-5-5", Usage: sessiondata.Usage{Input: 2, CacheRead: 7634, Output: 40}},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("requests %+v", got)
	}
	kept := string(w.Bytes())
	if strings.Contains(kept, "stream_event") || !strings.Contains(kept, `"type":"result"`) || !strings.Contains(kept, `"type":"assistant"`) {
		t.Fatalf("kept %s", kept)
	}
}

// codex says what a call consumed only when it ends, as one total.
func TestCodexReportsTheWholeCall(t *testing.T) {
	var got []Request
	w := &lineWriter{line: codexRequests(func(r Request) { got = append(got, r) })}
	_, _ = w.Write([]byte(`{"type":"item.completed"}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":119483,"cached_input_tokens":82944,"cache_write_input_tokens":0,"output_tokens":1563}}` + "\n"))
	if len(got) != 1 || !got[0].Whole || got[0].Usage != (sessiondata.Usage{Input: 36539, CacheRead: 82944, Output: 1563}) {
		t.Fatalf("requests %+v", got)
	}
}

// fakeCLI writes a shell script that prints lines and exits with a code. For
// codex it also writes the answer file named after -o.
func fakeCLI(t *testing.T, lines []string, code int) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	_ = os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	script := filepath.Join(dir, "cli")
	body := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"-o\" ]; then echo '{\"targets\":[]}' > \"$2\"; fi; shift; done\ncat " + out + "\nexit " + fmt.Sprint(code) + "\n"
	_ = os.WriteFile(script, []byte(body), 0o700)
	return script
}

// A call that fails keeps what was counted of it: its requests and, when the
// CLI said it, its usage. Only the answer fails.
func TestAFailedCallKeepsWhatItConsumed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	script := fakeCLI(t, []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":100}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":7}}}`,
		`{"type":"result","is_error":true,"subtype":"error_max_turns","usage":{"input_tokens":5,"cache_read_input_tokens":100,"output_tokens":7},"total_cost_usd":0.01}`,
	}, 1)
	ex, _ := NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script})
	res, err := ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if err == nil || len(res.Requests) != 1 || res.Usage == nil || res.Usage.Output != 7 || res.CostUSD == nil {
		t.Fatalf("result %+v usage %+v, error %v", res, res.Usage, err)
	}
}

// codex says what a call consumed but not on which model; the model it was
// told to use is what its usage is priced by.
func TestCodexUsageCarriesItsModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	script := fakeCLI(t, []string{`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}}`}, 0)
	ex, _ := NewExecutor("x", config.Evaluator{Kind: "cli", Program: "codex", Command: script, Model: "gpt-6.1-sol"})
	res, err := ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if err != nil || res.Model != "gpt-6.1-sol" || res.Usage == nil || res.Usage.CacheRead != 4 {
		t.Fatalf("result %+v %v", res, err)
	}
}

// Calls of one stage run at once on one API executor, each with its own
// navigation; run under the race detector, as make test does, this shows
// they share nothing they write.
func TestAnAPIEvaluatorTakesCallsAtOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","id":"u","name":"submit","input":`+submitted+`}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	ex, err := NewExecutor("e", config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	base := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nav := base.Fork()
			nav.ResetBudget(1 << 20)
			_, errs[i] = ex.Run(context.Background(), Task{Stage: "targets", Prompt: "judge", Schema: json.RawMessage(`{"type":"object"}`),
				Dir: t.TempDir(), Nav: nav})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A provider that echoes the key it refused never puts the key in the
// error, which reaches the log and the exported stage note.
func TestARefusedKeyIsNotRepeated(t *testing.T) {
	echo := func(secrets ...string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"refused: `+strings.Join(secrets, " ")+`"}`)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	// A key of any length, a short one too.
	for _, key := range []string{"sk-secret-0123456789", "k7q"} {
		srv := echo(key)
		t.Setenv("JUDGE_TEST_KEY", key)
		for _, program := range []string{"anthropic", "openai", "bedrock"} {
			_, err := runAPI(t, config.Evaluator{Kind: "api", Program: program, Endpoint: srv.URL, Model: "m", APIKeyEnv: "JUDGE_TEST_KEY"})
			if err == nil || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("%s with %q: %v", program, key, err)
			}
		}
	}
	// A signed Bedrock request: its access key, secret and token.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI")
	t.Setenv("AWS_SESSION_TOKEN", "TOKENEXAMPLE")
	srv := echo("AKIDEXAMPLE", "wJalrXUtnFEMI", "TOKENEXAMPLE")
	_, err := runAPI(t, config.Evaluator{Kind: "api", Program: "bedrock", Endpoint: srv.URL, Model: "m"})
	for _, secret := range []string{"AKIDEXAMPLE", "wJalrXUtnFEMI", "TOKENEXAMPLE"} {
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("a signed request: %v", err)
		}
	}
}

// A claude call stopped before its result keeps the usage its stream said of
// each request that ended, and counts apart the one that started and never
// ended.
func TestAnInterruptedClaudeCallKeepsWhatItsStreamSaid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	script := fakeCLI(t, []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":100}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":7}}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":3,"cache_read_input_tokens":200}}}}`,
	}, 1)
	ex, _ := NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script})
	res, err := ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if err == nil || res.Usage == nil || *res.Usage != (sessiondata.Usage{Input: 5, CacheRead: 100, Output: 7}) || res.UnknownRequests != 1 {
		t.Fatalf("usage %+v, %d unknown, %v", res.Usage, res.UnknownRequests, err)
	}
	// Each by the model it named, so it is priced at that model's rate.
	if res.Model != "claude-opus-5-5" || res.ByModel["claude-opus-5-5"] != *res.Usage {
		t.Fatalf("model %q, by model %+v", res.Model, res.ByModel)
	}
	// Stopped before any request ended: none is counted, the one that
	// started is counted apart, and what is known is known.
	script = fakeCLI(t, []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":3}}}}`,
	}, 1)
	ex, _ = NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script})
	res, _ = ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if res.Usage == nil || *res.Usage != (sessiondata.Usage{}) || res.UnknownRequests != 1 {
		t.Fatalf("before any request ended: usage %+v, %d unknown", res.Usage, res.UnknownRequests)
	}
	// Stopped before its stream showed anything: a request may have been on
	// its way, so one is counted apart, never taken for none.
	script = fakeCLI(t, []string{`{"type":"system","subtype":"init"}`}, 1)
	ex, _ = NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script})
	res, _ = ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if res.Usage == nil || res.UnknownRequests != 1 {
		t.Fatalf("before the stream showed a request: usage %+v, %d unknown", res.Usage, res.UnknownRequests)
	}
	// A request whose stream named no model takes the configured one.
	script = fakeCLI(t, []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":4}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":2}}}`,
	}, 1)
	ex, _ = NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script, Model: "configured"})
	res, _ = ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if res.Model != "configured" || res.ByModel["configured"].Input != 4 {
		t.Fatalf("model %q, by model %+v", res.Model, res.ByModel)
	}
}

// A CLI that exits while a process it started still holds its output is
// not waited for beyond pipeWait.
func TestACLIsLeftoverProcessDoesNotHoldTheCall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	old := pipeWait
	pipeWait = 200 * time.Millisecond
	t.Cleanup(func() { pipeWait = old })
	script := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5 &\necho '{\"type\":\"result\",\"structured_output\":{}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ex, _ := NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script})
	began := time.Now()
	_, _ = ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("the call waited %v for a leftover process", took)
	}
}

// A CLI that runs past its time is stopped with everything it started, and
// what its stream said is kept: the request still running is counted apart.
func TestATimedOutCLIIsStoppedWhole(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script, and a process group is a Unix one")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "cli")
	start := `{"type":"stream_event","event":{"type":"message_start","message":{"model":"m","usage":{"input_tokens":3}}}}`
	body := "#!/bin/sh\nsleep 30 &\necho '" + start + "'\necho $! > " + pidFile + ".new\nmv " + pidFile + ".new " + pidFile + "\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	ex, _ := NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script})
	// The call is stopped once the CLI has started its child, never at a
	// fixed time: in a full test run under the race detector, the shell took
	// more than 500 ms to get that far.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(pidFile); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	res, err := ex.Run(ctx, Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if err == nil || res.Usage == nil || res.UnknownRequests != 1 {
		t.Fatalf("usage %+v, %d unknown, %v", res.Usage, res.UnknownRequests, err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	p, _ := os.FindProcess(pid)
	for i := 0; i < 40; i++ {
		if p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = p.Kill()
	t.Fatalf("the CLI's child %d outlived it", pid)
}

// A codex call stopped before it said what it consumed counts a request
// apart, so the usage its stage knows, a retry's among it, stays known.
func TestAStoppedCodexCallCountsARequestApart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	script := fakeCLI(t, []string{`{"type":"thread.started"}`}, 1)
	ex, _ := NewExecutor("x", config.Evaluator{Kind: "cli", Program: "codex", Command: script, Model: "m"})
	res, err := ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if err == nil || res.Usage == nil || res.UnknownRequests != 1 {
		t.Fatalf("usage %+v, %d unknown, %v", res.Usage, res.UnknownRequests, err)
	}
}

// An API evaluator that lacks what its requests need is refused when it is
// made, before any stage runs. Every API needs a model, and Anthropic and
// OpenAI need an endpoint too. Bedrock makes its endpoint from the region, so
// a model alone is enough there.
func TestAnAPIEvaluatorWithoutAModelOrEndpointIsRefused(t *testing.T) {
	for _, tc := range []struct {
		conf config.Evaluator
		want string
	}{
		{config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: "https://api.example"}, "an API needs endpoint and model"},
		{config.Evaluator{Kind: "api", Program: "openai", Model: "m"}, "an API needs endpoint and model"},
		{config.Evaluator{Kind: "api", Program: "bedrock", Endpoint: "https://bedrock.example"}, "Bedrock needs a model id"},
	} {
		if ex, err := NewExecutor("e", tc.conf); ex != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: made %v, error %v", tc.conf, ex, err)
		}
	}
	if _, err := NewExecutor("e", config.Evaluator{Kind: "api", Program: "bedrock", Model: "m"}); err != nil {
		t.Fatalf("Bedrock with a model and no endpoint: %v", err)
	}
}

// An api_key_env that names an empty variable is a setup mistake. The stage
// fails and says which variable, and no request goes out without the key.
func TestAnEmptyAPIKeyStopsTheStageBeforeAnyRequest(t *testing.T) {
	answer := `{"content":[{"type":"tool_use","id":"u1","name":"submit","input":` + submitted + `}],"usage":{"input_tokens":1,"output_tokens":1}}`
	srv, bodies := fakeAPI(t, answer, answer)
	t.Setenv("JUDGE_EMPTY_KEY", "")
	res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m", APIKeyEnv: "JUDGE_EMPTY_KEY"})
	if err == nil || !strings.Contains(err.Error(), "JUDGE_EMPTY_KEY is empty") || res.Output != nil {
		t.Fatalf("answer %s, error %v", res.Output, err)
	}
	if len(*bodies) != 0 {
		t.Fatalf("%d requests went out without the key", len(*bodies))
	}
}

// OpenAI accepts a tool result only after the assistant message that made
// the call. The second request carries the model's own turn, with its call,
// before the result that answers it.
func TestOpenAIIsSentTheCallBeforeItsResult(t *testing.T) {
	srv, bodies := fakeAPI(t,
		`{"choices":[{"message":{"content":"Reading the card.","tool_calls":[{"id":"c1","function":{"name":"card","arguments":"{}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"c2","function":{"name":"submit","arguments":`+mustQuote(submitted)+`}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	if _, err := runAPI(t, config.Evaluator{Kind: "api", Program: "openai", Endpoint: srv.URL, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	second := (*bodies)[1]
	var req struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(second[strings.Index(second, "{"):]), &req); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	if !slices.Equal(roles, []string{"user", "assistant", "tool"}) {
		t.Fatalf("roles %v in %s", roles, second)
	}
	said, result := req.Messages[1], req.Messages[2]
	if said.Content != "Reading the card." || len(said.ToolCalls) != 1 || said.ToolCalls[0].ID != "c1" || said.ToolCalls[0].Type != "function" ||
		said.ToolCalls[0].Function.Name != "card" || said.ToolCalls[0].Function.Arguments != "{}" || result.ToolCallID != "c1" {
		t.Fatalf("the assistant turn %+v, the result %+v", said, result)
	}
}

// An OpenAI answer with no choice holds no turn to read. The stage fails and
// says so, and the request is counted as one whose usage is unknown.
func TestAnOpenAIAnswerWithNoChoiceFails(t *testing.T) {
	empty := `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":0}}`
	srv, bodies := fakeAPI(t, empty, empty)
	res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "openai", Endpoint: srv.URL, Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "no choice in the answer") || res.Output != nil || res.UnknownRequests != 1 || len(*bodies) != 1 {
		t.Fatalf("answer %s, %d unknown after %d requests, error %v", res.Output, res.UnknownRequests, len(*bodies), err)
	}
}

// claude marks a call it stopped, such as one out of turns, with is_error.
// Its result text can still hold an object, a draft the model wrote before
// it stopped. That draft is never taken for the answer, even when the CLI
// exits with 0. The call fails with the reason claude gave, and keeps what
// it consumed.
func TestAClaudeResultMarkedAnErrorIsNoAnswer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in CLI is a shell script")
	}
	script := fakeCLI(t, []string{
		`{"type":"result","is_error":true,"subtype":"error_max_turns","result":"Out of turns. So far: {\"targets\":[]}","usage":{"input_tokens":5,"output_tokens":7},"total_cost_usd":0.01}`,
	}, 0)
	ex, _ := NewExecutor("c", config.Evaluator{Kind: "cli", Program: "claude", Command: script})
	res, err := ex.Run(context.Background(), Task{Stage: "targets", Prompt: "p", Schema: json.RawMessage(`{}`), Dir: t.TempDir(), MCP: []string{"asz"}})
	if err == nil || !strings.Contains(err.Error(), "claude: error_max_turns: Out of turns.") || res.Output != nil {
		t.Fatalf("answer %s, error %v", res.Output, err)
	}
	if res.Usage == nil || res.Usage.Output != 7 || res.CostUSD == nil || *res.CostUSD != 0.01 {
		t.Fatalf("usage %+v, cost %v", res.Usage, res.CostUSD)
	}
}

// Models that cost the same are told apart by name, so the model named does
// not depend on the order a map is read in. Each parse reads the models in
// an order of its own, and every one names the same model.
func TestModelsOfEqualCostAreToldApartByName(t *testing.T) {
	out := []byte(`{"type":"result","is_error":false,"structured_output":{"targets":[]},"usage":{"input_tokens":1},
	  "modelUsage":{"m-d":{"inputTokens":1,"costUSD":0.1},"m-b":{"inputTokens":1,"costUSD":0.1},"m-f":{"inputTokens":1,"costUSD":0.1},
	                "m-a":{"inputTokens":1,"costUSD":0.1},"m-e":{"inputTokens":1,"costUSD":0.1},"m-c":{"inputTokens":1,"costUSD":0.1}}}`)
	for range 20 {
		res, err := claudeResult(out)
		if err != nil {
			t.Fatal(err)
		}
		if res.Model != "m-a" {
			t.Fatalf("named %s of six models that cost the same", res.Model)
		}
	}
}

// A claude result whose structured output is null gave no structured
// answer: the answer is the object at the end of its text.
func TestANullStructuredOutputFallsBackToTheText(t *testing.T) {
	res, err := claudeResult([]byte(`{"type":"result","is_error":false,"structured_output":null,"result":"Done. {\"targets\":[],\"inputs\":[]}"}`))
	if err != nil || string(res.Output) != `{"targets":[],"inputs":[]}` {
		t.Fatalf("answer %s, error %v", res.Output, err)
	}
}

// A usage the CLIs report with a field of the wrong type is not read at all,
// rather than counted from the fields that happened to decode: a claude
// request that starts that way is not counted, and a codex event is not
// taken for usage.
func TestAUsageOfTheWrongTypeIsNotRead(t *testing.T) {
	var got []Request
	starts := 0
	read := claudeRequests(func(r Request) { got = append(got, r) }, func() { starts++ })
	read([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"model":"m","usage":{"input_tokens":"many","cache_read_input_tokens":9}}}}`))
	read([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":4}}}`))
	if starts != 0 || len(got) != 0 {
		t.Fatalf("a request of the wrong type started %d times and was counted as %+v", starts, got)
	}
	if u, ok := codexTurnUsage([]byte(`{"type":"turn.completed","usage":{"input_tokens":"many","output_tokens":5}}`)); ok {
		t.Fatalf("a codex usage of the wrong type was read as %+v", u)
	}
}

// A claude result with a usage of the wrong type does not read, even with a
// valid structured answer beside it: an answer is taken only with what it
// cost.
func TestAClaudeResultOfTheWrongTypeIsRefused(t *testing.T) {
	res, err := claudeResult([]byte(`{"type":"result","is_error":false,"structured_output":{"targets":[]},"usage":{"input_tokens":"many"}}`))
	if err == nil || !strings.Contains(err.Error(), "the result does not read") || res.Output != nil {
		t.Fatalf("answer %s, error %v", res.Output, err)
	}
}

// Without a cost from claude, the model that read and wrote the most tokens
// is the one named, though another comes first by name.
func TestTokensNameTheModelWhenNoCostIsGiven(t *testing.T) {
	res, err := claudeResult([]byte(`{"type":"result","is_error":false,"structured_output":{"targets":[]},"usage":{"input_tokens":1},
	  "modelUsage":{"m-a":{"inputTokens":10},"m-z":{"inputTokens":900,"outputTokens":100}}}`))
	if err != nil || res.Model != "m-z" {
		t.Fatalf("named %q, error %v", res.Model, err)
	}
}

// claude can print other JSON lines before its result and a warning after
// it: the result is the last line that is JSON.
func TestTheResultIsTheLastJSONLine(t *testing.T) {
	res, err := claudeResult([]byte(`{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"result","is_error":false,"structured_output":{"targets":[]},"usage":{"input_tokens":3}}` + "\n" +
		"warning: a plugin took long to load\n"))
	if err != nil || string(res.Output) != `{"targets":[]}` || res.Usage == nil || res.Usage.Input != 3 {
		t.Fatalf("answer %s, usage %+v, error %v", res.Output, res.Usage, err)
	}
}

// A codex call that wrote no answer file says so, and keeps what it
// consumed: it is not taken for an answer with no JSON in it.
func TestACodexCallWithNoAnswerFileSaysSo(t *testing.T) {
	res, err := codexResult([]byte(`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3}}`), filepath.Join(t.TempDir(), "absent.json"))
	if err == nil || !strings.Contains(err.Error(), "codex: no answer:") || res.Usage == nil || res.Usage.Input != 12 {
		t.Fatalf("usage %+v, error %v", res.Usage, err)
	}
}

// A reply that reports no usage still answers: its request is counted apart
// as one whose usage is unknown, for OpenAI and for Bedrock alike.
func TestAReplyWithoutUsageCountsARequestApart(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		srv, _ := fakeAPI(t,
			`{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"card","arguments":"{}"}}]}}]}`,
			`{"choices":[{"message":{"tool_calls":[{"id":"c2","function":{"name":"submit","arguments":`+mustQuote(submitted)+`}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
		res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "openai", Endpoint: srv.URL, Model: "m"})
		if err != nil || string(res.Output) != submitted || res.UnknownRequests != 1 {
			t.Fatalf("answer %s, %d unknown, error %v", res.Output, res.UnknownRequests, err)
		}
	})
	t.Run("bedrock", func(t *testing.T) {
		srv, _ := fakeAPI(t,
			`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"b1","name":"card","input":{}}}]}}}`,
			`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"b2","name":"submit","input":`+submitted+`}}]}},"usage":{"inputTokens":4,"outputTokens":2}}`)
		t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
		t.Setenv("AWS_SESSION_TOKEN", "")
		res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "bedrock", Endpoint: srv.URL, Model: "m"})
		if err != nil || string(res.Output) != submitted || res.UnknownRequests != 1 {
			t.Fatalf("answer %s, %d unknown, error %v", res.Output, res.UnknownRequests, err)
		}
	})
}

// A Bedrock reply whose usage has a field of the wrong type does not read,
// even with a valid submit call in it.
func TestABedrockReplyOfTheWrongTypeIsRefused(t *testing.T) {
	answer := `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"b2","name":"submit","input":` + submitted + `}}]}},"usage":{"inputTokens":"many"}}`
	srv, _ := fakeAPI(t, answer, answer)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	res, err := runAPI(t, config.Evaluator{Kind: "api", Program: "bedrock", Endpoint: srv.URL, Model: "m"})
	if err == nil || res.Output != nil {
		t.Fatalf("answer %s, error %v", res.Output, err)
	}
}

// A tool call with no arguments is sent back as one with an empty object:
// the API refuses an empty string where it expects JSON.
func TestEmptyToolArgumentsAreSentBackAsAnObject(t *testing.T) {
	srv, bodies := fakeAPI(t,
		`{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"card","arguments":""}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"c2","function":{"name":"submit","arguments":`+mustQuote(submitted)+`}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	if _, err := runAPI(t, config.Evaluator{Kind: "api", Program: "openai", Endpoint: srv.URL, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*bodies)[1], `"arguments":"{}"`) {
		t.Fatalf("the call was sent back as %s", (*bodies)[1])
	}
}
