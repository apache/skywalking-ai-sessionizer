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

package index

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// magic identifies an index file and guards against reading an unrelated one.
var magic = [4]byte{'A', 'S', 'I', 'X'}

const (
	// seq row stream batch kind trigger flags ts  record parent call run continues tool child started_by label  first count
	entryWidth = 4 + 4 + 4 + 4 + 1 + 1 + 2 + 8 + 4*9 + 4 + 4 // 72
	blockWidth = 4 + 2 + 1 + 4 + 4                           // 15
)

var le = binary.LittleEndian

// Write persists the index to dir as entries.bin.
//
// The format is a flat block of fixed-width records after an interned string
// table. Lookup maps are not persisted - rebuilding them all takes about 4 ms
// for the largest observed session, which is cheaper than the format complexity
// of storing them and cannot go stale.
func (ix *Index) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "entries.bin")
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()

	w := bufio.NewWriterSize(f, 1<<20)
	if _, err := w.Write(magic[:]); err != nil {
		return err
	}
	if err := writeU32(w, Schema); err != nil {
		return err
	}

	strs := ix.Strings.all()
	if err := writeU32(w, uint32(len(strs))); err != nil {
		return err
	}
	for _, s := range strs {
		if len(s) > 0xffff {
			return fmt.Errorf("index: identifier too long (%d bytes)", len(s))
		}
		if err := binary.Write(w, le, uint16(len(s))); err != nil {
			return err
		}
		if _, err := w.WriteString(s); err != nil {
			return err
		}
	}

	if err := writeU32(w, uint32(len(ix.Entries))); err != nil {
		return err
	}
	buf := make([]byte, entryWidth)
	for i := range ix.Entries {
		encodeEntry(buf, &ix.Entries[i])
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}

	if err := writeU32(w, uint32(len(ix.Blocks))); err != nil {
		return err
	}
	bbuf := make([]byte, blockWidth)
	for i := range ix.Blocks {
		encodeBlock(bbuf, &ix.Blocks[i])
		if _, err := w.Write(bbuf); err != nil {
			return err
		}
	}

	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads an index written by Write. A missing file, a foreign file or a
// schema mismatch returns ok=false rather than an error: the index is derived,
// so the caller rebuilds instead of migrating.
func Load(dir, session string) (ix *Index, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, "entries.bin"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(data) < 8 || string(data[:4]) != string(magic[:]) {
		return nil, false, nil
	}
	p := 4
	if le.Uint32(data[p:]) != Schema {
		return nil, false, nil
	}
	p += 4

	read := func(n int) ([]byte, error) {
		if p+n > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		b := data[p : p+n]
		p += n
		return b, nil
	}

	b, err := read(4)
	if err != nil {
		return nil, false, err
	}
	nStr := int(le.Uint32(b))
	strs := make([]string, nStr)
	for i := 0; i < nStr; i++ {
		if b, err = read(2); err != nil {
			return nil, false, err
		}
		l := int(le.Uint16(b))
		if b, err = read(l); err != nil {
			return nil, false, err
		}
		strs[i] = string(b)
	}

	if b, err = read(4); err != nil {
		return nil, false, err
	}
	nEnt := int(le.Uint32(b))
	entries := make([]Entry, nEnt)
	for i := 0; i < nEnt; i++ {
		if b, err = read(entryWidth); err != nil {
			return nil, false, err
		}
		decodeEntry(b, &entries[i])
	}

	if b, err = read(4); err != nil {
		return nil, false, err
	}
	nBlk := int(le.Uint32(b))
	blocks := make([]Block, nBlk)
	for i := 0; i < nBlk; i++ {
		if b, err = read(blockWidth); err != nil {
			return nil, false, err
		}
		decodeBlock(b, &blocks[i])
	}

	return &Index{
		Session: session, Strings: loadInterner(strs),
		Entries: entries, Blocks: blocks,
	}, true, nil
}

func writeU32(w io.Writer, v uint32) error { return binary.Write(w, le, v) }

func encodeEntry(b []byte, e *Entry) {
	le.PutUint32(b[0:], e.Seq)
	le.PutUint32(b[4:], e.Row)
	le.PutUint32(b[8:], e.Stream)
	le.PutUint32(b[12:], e.Batch)
	b[16] = byte(e.Kind)
	b[17] = byte(e.Trigger)
	le.PutUint16(b[18:], uint16(e.Flags))
	le.PutUint64(b[20:], uint64(e.TS))
	le.PutUint32(b[28:], e.Record)
	le.PutUint32(b[32:], e.Parent)
	le.PutUint32(b[36:], e.Call)
	le.PutUint32(b[40:], e.Run)
	le.PutUint32(b[44:], e.Continues)
	le.PutUint32(b[48:], e.Tool)
	le.PutUint32(b[52:], e.Child)
	le.PutUint32(b[56:], e.StartedBy)
	le.PutUint32(b[60:], e.Label)
	le.PutUint32(b[64:], e.BlockFirst)
	le.PutUint32(b[68:], e.BlockCount)
}

func decodeEntry(b []byte, e *Entry) {
	e.Seq = le.Uint32(b[0:])
	e.Row = le.Uint32(b[4:])
	e.Stream = le.Uint32(b[8:])
	e.Batch = le.Uint32(b[12:])
	e.Kind = Kind(b[16])
	e.Trigger = Trigger(b[17])
	e.Flags = Flags(le.Uint16(b[18:]))
	e.TS = int64(le.Uint64(b[20:]))
	e.Record = le.Uint32(b[28:])
	e.Parent = le.Uint32(b[32:])
	e.Call = le.Uint32(b[36:])
	e.Run = le.Uint32(b[40:])
	e.Continues = le.Uint32(b[44:])
	e.Tool = le.Uint32(b[48:])
	e.Child = le.Uint32(b[52:])
	e.StartedBy = le.Uint32(b[56:])
	e.Label = le.Uint32(b[60:])
	e.BlockFirst = le.Uint32(b[64:])
	e.BlockCount = le.Uint32(b[68:])
}

func encodeBlock(b []byte, k *Block) {
	le.PutUint32(b[0:], k.Entry)
	le.PutUint16(b[4:], k.Ord)
	b[6] = byte(k.Kind)
	le.PutUint32(b[7:], k.ToolID)
	le.PutUint32(b[11:], k.Name)
}

func decodeBlock(b []byte, k *Block) {
	k.Entry = le.Uint32(b[0:])
	k.Ord = le.Uint16(b[4:])
	k.Kind = BlockKind(b[6])
	k.ToolID = le.Uint32(b[7:])
	k.Name = le.Uint32(b[11:])
}
