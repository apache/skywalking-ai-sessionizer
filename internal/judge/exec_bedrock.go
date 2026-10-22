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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// awsCredentials signs a request to Bedrock.
type awsCredentials struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
}

// credentials come from the environment, or from the AWS CLI, which resolves
// a profile, an SSO login and its refresh the way every AWS tool does. asz
// keeps none of them.
func (a *apiExecutor) credentials(ctx context.Context) (awsCredentials, error) {
	if id, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		return awsCredentials{id, secret, os.Getenv("AWS_SESSION_TOKEN")}, nil
	}
	args := []string{"configure", "export-credentials", "--format", "process"}
	if a.conf.Profile != "" {
		args = append(args, "--profile", a.conf.Profile)
	}
	out, err := exec.CommandContext(ctx, "aws", args...).Output()
	if err != nil {
		msg := err.Error()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg = lastLine(string(ee.Stderr))
		}
		return awsCredentials{}, fmt.Errorf("%s: no AWS credentials: %s", a.desc.Name, msg)
	}
	var c awsCredentials
	if err := json.Unmarshal(out, &c); err != nil || c.AccessKeyID == "" {
		return awsCredentials{}, fmt.Errorf("%s: the AWS CLI gave no credentials", a.desc.Name)
	}
	return c, nil
}

// callBedrock speaks Bedrock's Converse API. With a key, a Bedrock API key,
// the request carries it as a bearer token; without one, it is signed with
// Signature Version 4 from AWS credentials.
func (a *apiExecutor) callBedrock(ctx context.Context, key string, turns []turn, tools []Tool) (turn, *sessiondata.Usage, error) {
	type block map[string]any
	var msgs []map[string]any
	for _, t := range turns {
		var content []block
		if t.text != "" {
			content = append(content, block{"text": t.text})
		}
		for _, c := range t.calls {
			content = append(content, block{"toolUse": block{"toolUseId": c.ID, "name": c.Name, "input": json.RawMessage(nonEmpty(c.Args))}})
		}
		for _, r := range t.results {
			res := block{"toolUseId": r.ID, "content": []block{{"text": r.Text}}}
			if r.Error {
				res["status"] = "error"
			}
			content = append(content, block{"toolResult": res})
		}
		msgs = append(msgs, map[string]any{"role": t.role, "content": content})
	}
	var specs []block
	for _, tool := range tools {
		specs = append(specs, block{"toolSpec": block{"name": tool.Name, "description": tool.Description,
			"inputSchema": block{"json": tool.InputSchema}}})
	}
	body, err := json.Marshal(map[string]any{"messages": msgs, "toolConfig": block{"tools": specs},
		"inferenceConfig": block{"maxTokens": a.maxTokens()}})
	if err != nil {
		return turn{}, nil, err
	}
	region := a.conf.Region
	if region == "" {
		region = "us-east-1"
	}
	endpoint := strings.TrimRight(a.conf.Endpoint, "/")
	if endpoint == "" {
		endpoint = "https://bedrock-runtime." + region + ".amazonaws.com"
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return turn{}, nil, err
	}
	// The model id goes into the path escaped, since ids such as
	// "us.deepseek.r1-v1:0" hold a colon, and an ARN holds slashes.
	path := "/model/" + awsEscape(a.conf.Model) + "/converse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(body))
	if err != nil {
		return turn{}, nil, err
	}
	req.URL.RawPath = path
	req.Header.Set("Content-Type", "application/json")
	secrets := []string{key}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		creds, err := a.credentials(ctx)
		if err != nil {
			return turn{}, nil, err
		}
		sign(req, body, creds, region, "bedrock", base.Host, time.Now().UTC())
		secrets = append(secrets, creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return turn{}, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return turn{}, nil, err
	}
	if resp.StatusCode/100 != 2 {
		return turn{}, nil, fmt.Errorf("%s: %s: %s", a.desc.Name, resp.Status, clip(redact(string(data), secrets...), 300))
	}
	var out struct {
		Output struct {
			Message struct {
				Content []struct {
					Text    string `json:"text"`
					ToolUse *struct {
						ID    string          `json:"toolUseId"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
					} `json:"toolUse"`
				} `json:"content"`
			} `json:"message"`
		} `json:"output"`
		Usage *struct {
			Input      int `json:"inputTokens"`
			Output     int `json:"outputTokens"`
			CacheRead  int `json:"cacheReadInputTokens"`
			CacheWrite int `json:"cacheWriteInputTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return turn{}, nil, err
	}
	t := turn{role: "assistant"}
	for _, c := range out.Output.Message.Content {
		t.text += c.Text
		if c.ToolUse != nil {
			t.calls = append(t.calls, toolCall{ID: c.ToolUse.ID, Name: c.ToolUse.Name, Args: c.ToolUse.Input})
		}
	}
	if out.Usage == nil {
		return t, nil, nil
	}
	u := out.Usage
	return t, &sessiondata.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}, nil
}

// sign adds a Signature Version 4 Authorization header. Every path segment is
// escaped a second time in the canonical request, as the signature asks of
// every service but S3, so a model id's colon, sent as %3A, is signed as
// %253A.
func sign(req *http.Request, body []byte, c awsCredentials, region, service, host string, now time.Time) {
	date := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	payload := sha256.Sum256(body)
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", date)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	headers := map[string]string{"host": host, "x-amz-date": date, "content-type": req.Header.Get("Content-Type")}
	if c.SessionToken != "" {
		headers["x-amz-security-token"] = c.SessionToken
	}
	var names []string
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	segments := strings.Split(req.URL.EscapedPath(), "/")
	for i, s := range segments {
		segments[i] = awsEscape(s)
	}
	canon := strings.Join([]string{req.Method, strings.Join(segments, "/"), req.URL.RawQuery,
		canonHeaders.String(), signed, hex.EncodeToString(payload[:])}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	hashed := sha256.Sum256([]byte(canon))
	toSign := "AWS4-HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(hashed[:])
	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

// awsEscape escapes every byte but the unreserved characters of RFC 3986,
// as Signature Version 4 requires. Go's url.PathEscape leaves characters such
// as the colon, which the signature needs escaped.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
