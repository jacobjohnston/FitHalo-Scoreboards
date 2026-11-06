package store

import (
	"bytes"
	"compress/flate"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"sync"
)

// dictionary1 is typical board documents, which DEFLATE refers back to so even a small document compresses.
// It must never change: documents stored with formatDeflate1 can only be read with these exact bytes.
//
//go:embed dictionary-1.txt
var dictionary1 []byte

const (
	// formatPlain marks a document stored as written.
	formatPlain byte = 1

	// formatDeflate1 marks a document compressed with DEFLATE against dictionary1.
	formatDeflate1 byte = 2

	// maximumExpandedBytes bounds what one stored document may decompress to.
	maximumExpandedBytes = 8 << 20
)

// errMalformedDocument means a stored document's format byte or compressed data is not readable.
var errMalformedDocument = errors.New("store: a stored document is malformed")

// writers holds DEFLATE writers primed with dictionary1, since each costs hundreds of kilobytes to make.
var writers = sync.Pool{New: func() any {
	writer, err := flate.NewWriterDict(io.Discard, flate.BestCompression, dictionary1)
	if err != nil {
		panic(err)
	}
	return writer
}}

// readers holds DEFLATE readers, reset to dictionary1 before each use.
var readers = sync.Pool{New: func() any { return flate.NewReaderDict(bytes.NewReader(nil), dictionary1) }}

// Compressed keeps every document of the store it wraps in whichever form is smaller: as written, or
// compressed. Callers see only the documents as written.
type Compressed struct {
	// Store is the backend the documents are kept in.
	Store Store
}

// Compress wraps backend so its documents are stored compressed where that makes them smaller.
func Compress(backend Store) *Compressed {
	return &Compressed{Store: backend}
}

// Get returns a document as it was written, or ErrNotFound.
func (c *Compressed) Get(ctx context.Context, collection, id string) (Document, error) {
	document, err := c.Store.Get(ctx, collection, id)
	if err != nil {
		return Document{}, err
	}
	if document.Data, err = expand(document.Data); err != nil {
		return Document{}, fmt.Errorf("%w: %s/%s", err, collection, id)
	}
	return document, nil
}

// Put stores a document in its smaller form if its version is still expectedVersion.
func (c *Compressed) Put(ctx context.Context, collection, id string, data []byte, expectedVersion int64) (int64, error) {
	packed, err := pack(data)
	if err != nil {
		return 0, err
	}
	return c.Store.Put(ctx, collection, id, packed, expectedVersion)
}

// Delete removes a document if its version is still expectedVersion.
func (c *Compressed) Delete(ctx context.Context, collection, id string, expectedVersion int64) error {
	return c.Store.Delete(ctx, collection, id, expectedVersion)
}

// List returns every document in a collection as it was written, in identifier order.
func (c *Compressed) List(ctx context.Context, collection string) ([]Item, error) {
	items, err := c.Store.List(ctx, collection)
	if err != nil {
		return nil, err
	}
	for index := range items {
		if items[index].Data, err = expand(items[index].Data); err != nil {
			return nil, fmt.Errorf("%w: %s/%s", err, collection, items[index].ID)
		}
	}
	return items, nil
}

// Collections returns the collections directly beneath prefix.
func (c *Compressed) Collections(ctx context.Context, prefix string) ([]string, error) {
	return c.Store.Collections(ctx, prefix)
}

// DeleteCollection removes a collection and everything beneath it.
func (c *Compressed) DeleteCollection(ctx context.Context, collection string) error {
	return c.Store.DeleteCollection(ctx, collection)
}

// Close releases the wrapped store.
func (c *Compressed) Close() error {
	return c.Store.Close()
}

// pack returns data behind a format byte: compressed when that is smaller, as written otherwise.
func pack(data []byte) ([]byte, error) {
	var compressed bytes.Buffer
	compressed.WriteByte(formatDeflate1)
	writer := writers.Get().(*flate.Writer)
	defer func() {
		writer.Reset(io.Discard)
		writers.Put(writer)
	}()
	writer.Reset(&compressed)
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() < len(data)+1 {
		return compressed.Bytes(), nil
	}
	return append([]byte{formatPlain}, data...), nil
}

// expand returns a packed document as it was written.
func expand(packed []byte) ([]byte, error) {
	if len(packed) == 0 {
		return nil, errMalformedDocument
	}
	switch packed[0] {
	case formatPlain:
		return packed[1:], nil
	case formatDeflate1:
		reader := readers.Get().(io.ReadCloser)
		defer readers.Put(reader)
		if err := reader.(flate.Resetter).Reset(bytes.NewReader(packed[1:]), dictionary1); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(reader, maximumExpandedBytes+1))
		if err != nil || len(data) > maximumExpandedBytes {
			return nil, errMalformedDocument
		}
		return data, nil
	}
	return nil, errMalformedDocument
}
