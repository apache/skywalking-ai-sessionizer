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

package mcpclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// annotation is one x-mcp-header a tool's schema declares: the header's
// name part, and the path of the property, by property names.
type annotation struct {
	name string
	path []string
}

// annotations reads the x-mcp-header annotations of a tool's input schema,
// as revision 2026-07-28 allows them: on a string, integer or boolean
// property reached from the root through properties alone, each a token,
// unique without regard to case. Anywhere else, or of any other shape, an
// annotation makes the tool invalid, and a client must leave it out.
func annotations(schema json.RawMessage) ([]annotation, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("an input schema that is not JSON")
	}
	var out []annotation
	seen := map[string]bool{}
	var walk func(node any, path []string, chain bool) error
	walk = func(node any, path []string, chain bool) error {
		switch v := node.(type) {
		case []any:
			for _, x := range v {
				if err := walk(x, path, false); err != nil {
					return err
				}
			}
		case map[string]any:
			if raw, ok := v["x-mcp-header"]; ok {
				name, isText := raw.(string)
				switch {
				case !chain || len(path) == 0:
					return fmt.Errorf("an x-mcp-header not on a property reached through properties alone")
				case !isText || !token(name):
					return fmt.Errorf("an x-mcp-header that is not a token")
				case seen[strings.ToLower(name)]:
					return fmt.Errorf("the x-mcp-header %q twice", name)
				}
				switch t, _ := v["type"].(string); t {
				case "string", "integer", "boolean":
				default:
					return fmt.Errorf("an x-mcp-header on a property of type %q", t)
				}
				seen[strings.ToLower(name)] = true
				out = append(out, annotation{name: name, path: append([]string(nil), path...)})
			}
			for k, x := range v {
				switch k {
				case "x-mcp-header":
				case "properties":
					props, ok := x.(map[string]any)
					if !ok {
						if err := walk(x, path, false); err != nil {
							return err
						}
						continue
					}
					for name, p := range props {
						if err := walk(p, append(append([]string(nil), path...), name), chain); err != nil {
							return err
						}
					}
				default:
					if err := walk(x, path, false); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(root, nil, true); err != nil {
		return nil, err
	}
	return out, nil
}

// token says whether a name is an HTTP field-name token.
func token(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}

// maxSafe is the largest integer a header may carry: the largest JavaScript
// reads exactly.
const maxSafe = 1<<53 - 1

// paramHeaders are the Mcp-Param headers of a call: each annotated argument's
// value, read at its path, a string as it is, an integer in decimal, a
// boolean as true or false; an argument absent or null gets none.
func paramHeaders(annots []annotation, args map[string]any) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range annots {
		var v any = args
		for _, p := range a.path {
			m, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = m[p]
		}
		where := strings.Join(a.path, ".")
		var text string
		switch x := v.(type) {
		case nil:
			continue
		case string:
			text = x
		case bool:
			text = fmt.Sprint(x)
		default:
			n, err := integer(x)
			if err != nil {
				return nil, failure(ClassProtocol, "argument %s %s", where, err)
			}
			text = fmt.Sprint(n)
		}
		out["Mcp-Param-"+a.name] = headerValue(text)
	}
	return out, nil
}

// integer reads an integer argument as configuration or JSON gives it, and
// keeps it within what a header may carry.
func integer(v any) (int64, error) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	case uint64:
		if x > maxSafe {
			return 0, fmt.Errorf("is past the integers a header carries")
		}
		n = int64(x)
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > maxSafe {
			return 0, fmt.Errorf("is not an integer a header carries")
		}
		n = int64(x)
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0, fmt.Errorf("is not an integer")
		}
		n = i
	default:
		return 0, fmt.Errorf("is neither text, an integer nor true or false")
	}
	if n > maxSafe || n < -maxSafe {
		return 0, fmt.Errorf("is past the integers a header carries")
	}
	return n, nil
}

// headerValue is a value as a header carries it: as it is when it is plain
// visible ASCII, spaces between, with no white space at its ends, and does
// not look like the sentinel; otherwise Base64 inside the sentinel. A tab
// or any other control character is not plain.
func headerValue(v string) string {
	sentinel := strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=")
	plain := v == strings.TrimSpace(v) && !sentinel
	for _, c := range v {
		if c != ' ' && (c < 0x21 || c > 0x7e) {
			plain = false
			break
		}
	}
	if plain {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}
