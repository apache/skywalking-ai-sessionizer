# The LangChain JS demo application

Two LangChain JS applications, captured through the same stand-in receiver as
the Python application in `../langchain`, so their captures sit in its
`testdata/` beside the Python ones and are read by the same tests. Neither needs
an API key or a provider: a stand-in model serves every call.

| case | file | what it exercises |
| --- | --- | --- |
| js-graph | `graph.js` | A LangGraph JS graph with one tool on a thread: every message serialized as a constructor. |
| js-traceable | `traceable.js` | LangSmith JS's `traceable` around a chat model: no `ls_method`, and no thread for the runs inside. |

## Capturing a case

The versions are pinned in `package.json`; a recapture with others is a new
measurement, and `../langchain/MEASUREMENTS.md` says what it holds.

```sh
npm install
ASZ_CAPTURE=../langchain/testdata/js-graph python3 ../langchain/recorder.py &
LANGSMITH_TRACING=true LANGSMITH_ENDPOINT=http://127.0.0.1:8930 \
  LANGSMITH_API_KEY=asz-harness LANGSMITH_PROJECT=js-graph node graph.js
kill %1
```

and the same for `traceable.js` into `testdata/js-traceable`, then
`python measure.py` in `../langchain` to write the measurements again.
