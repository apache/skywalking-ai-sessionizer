# Released writer fixture

`released-writer.sd` was written with `pkg/sessiondata/writer.go` from commit
`8fd56ff` under Go 1.27.1. It holds the first three records returned by
`encodingRecords` in `mixed_encoding_test.go`, using `encodingHeader(1)`.

The old writer used `json.Marshal`, which inserted HTML escapes into both
ordinary strings and raw part data. Keep this file unchanged when the writer
changes. The test lands it before a first parse, then adds a file written by
the current writer before a second parse.

The six records are synthetic model records. Their source identifiers, byte
lengths, offsets and SHA-256 values are fixed by the test. No user data or
runtime source files are needed.
