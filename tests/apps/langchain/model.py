# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

"""A stand-in for a model provider, speaking the OpenAI chat completions API.

The harness needs no API key, no network and no provider account, and every
run produces the same shape. Each case names the behaviour it wants in the
request's `model` field, so the script travels with the call and one server
serves every case.
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8931

# A command long enough to prove a tool's arguments are not truncated.
BIG_COMMAND = (
    "kubectl get pods -n istio-system -o jsonpath='{range .items[*]}"
    "{.metadata.name}{\"\\t\"}{.status.phase}{\"\\n\"}{end}' | " * 400
    + "grep -v Running  # the whole command, about 20 KB"
)

_calls = 0
_lock = threading.Lock()


def _tool_call(name, index, arguments=None):
    return {
        "id": "call_%s_%d" % (name, index),
        "type": "function",
        "function": {
            "name": name,
            "arguments": json.dumps(arguments if arguments is not None else {
                "cluster": "prod-1", "question": "check prod-1", "text": "some findings",
            }),
        },
    }


def _reply(behaviour, tools, tool_messages, index):
    """What the model says next, given the case and what it has been told."""
    if behaviour == "summary":
        return None, "summary %d: the clusters checked so far were healthy" % index
    if behaviour == "plain" or not tools:
        return None, "answer %d" % index
    if behaviour == "parallel" and tool_messages == 0:
        return [_tool_call(tools[0], index), _tool_call(tools[0], index + 1000)], None
    if behaviour == "loop":
        if tool_messages < 5:
            return [_tool_call(tools[0], index)], None
        return None, "looped %d times" % tool_messages
    if behaviour == "large" and tool_messages == 0:
        return [_tool_call(tools[0], index, {"command": BIG_COMMAND, "timeout": 30})], None
    if behaviour == "every-tool" and tool_messages < len(tools):
        return [_tool_call(tools[tool_messages], index)], None
    if behaviour in ("tool", "tool-error", "slow", "large") and tool_messages == 0:
        return [_tool_call(tools[0], index)], None
    return None, "answer %d" % index


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        global _calls
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with _lock:
            _calls += 1
            index = _calls
        behaviour = request.get("model", "tool")
        tools = [t["function"]["name"] for t in request.get("tools", [])]
        told = sum(1 for m in request.get("messages", []) if m.get("role") == "tool")
        calls, text = _reply(behaviour, tools, told, index)
        message = {"role": "assistant", "content": text}
        if calls:
            message = {"role": "assistant", "content": None, "tool_calls": calls}
        body = json.dumps({
            "id": "chatcmpl-%d" % index,
            "object": "chat.completion",
            "created": 1700000000,
            "model": behaviour,
            "choices": [{"index": 0, "message": message,
                         "finish_reason": "tool_calls" if calls else "stop"}],
            "usage": {"prompt_tokens": 100 + index, "completion_tokens": 10,
                      "total_tokens": 110 + index,
                      "prompt_tokens_details": {"cached_tokens": 5}},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def serve():
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()


if __name__ == "__main__":
    serve()
