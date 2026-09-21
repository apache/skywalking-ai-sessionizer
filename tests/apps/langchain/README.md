# The LangChain demo application

A real LangGraph application, one case per shape asz has to handle, captured
through a stand-in receiver. It needs no API key, no network and no provider
account: a stand-in model serves every call and the same run produces the same
shape every time.

## Running it

```sh
make langchain-capture          # every case, into testdata/
```

or, from this directory with the environment already made:

```sh
python run.py                   # every case
python run.py subagent loop     # only those
python run.py -l                # what the cases are
```

Each case runs in its own process and writes one directory per request under
`testdata/<case>/`, holding the request's headers, its parts byte for byte, and
a `meta.json` describing what arrived.

## What the pieces are

| File | Is |
| --- | --- |
| `agents.py` | the demo application: one function per case |
| `model.py` | the stand-in model, speaking the OpenAI chat completions API |
| `recorder.py` | the stand-in receiver, keeping what the client sent |
| `run.py` | starts the two servers and runs the cases |
| `case.py` | runs one case in its own process and flushes the client |

The model's behaviour travels in the request's `model` field, so a case reads
as the shape it produces rather than as a script, and one server serves them
all.

## Why the versions are pinned

A fixture records what one version of one client sent. `pyproject.toml` pins
exact versions so a capture is reproducible, and moving them is a deliberate
recapture rather than a surprise in a later run.
