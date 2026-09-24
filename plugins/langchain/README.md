# Apache SkyWalking AI Sessionizer — LangChain plugin

Records what each of a LangChain or LangGraph agent's tool calls changed on
disk, so a conversation shows the work as well as the words.

It is the smaller half of a pair: `asz-changes` does the scanning and the
diffing, and this says when a tool call begins and ends, which LangChain has no
way to say on its own. The application imports nothing.

```sh
pip install apache-skywalking-asz-langchain
asz-langchain enable
```

```sh
export ASZ_CHANGES=true
export ASZ_WATCH=/path/to/the/workspace
export ASZ_CHANGES_DATA=/var/lib/asz/changes
```

It needs `asz-changes` on `PATH`, which the install script, Homebrew and apt put
beside `asz`. Nothing is recorded until `tools.scope` in
`$ASZ_CHANGES_DATA/settings.yaml` names your application's tools.

**[Full setup, what it costs and what it cannot see](https://skywalking.apache.org/docs/skywalking-ai-sessionizer/latest/en/setup/langchain-plugin/)**

To collect the conversation itself, which needs no plugin at all, see
[LangChain and LangGraph](https://skywalking.apache.org/docs/skywalking-ai-sessionizer/latest/en/setup/langchain/).

Apache-2.0. Part of [Apache SkyWalking](https://skywalking.apache.org/).
