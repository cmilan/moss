//  Copyright (c) 2016 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

// Package moss stands for "memory-oriented sorted segments", and
// provides a data structure that manages an ordered Collection of
// key-val entries.
//
// The design is similar to a (much) simplified LSM tree, in that
// there is a stack of sorted key-val arrays or "segments".  To
// incorporate the next Batch (see: ExecuteBatch()), we sort the
// incoming Batch of key-val mutations into a "segment" and atomically
// push the new segment onto the stack.  A higher segment in the stack
// will shadow entries of the same key from lower segments.
//
// Separately, an asynchronous goroutine (the "merger") will
// continuously merge N sorted segments into a single sorted segment
// to keep stack height low.  After you stop mutations, that is, the
// stack will eventually be merged down into a stack of height 1.
//
// The remaining, single, large sorted segment will be efficient in
// memory usage and efficient for binary search and range iteration.
//
// Another asynchronous goroutine (the "persister") can optionally
// persist the most recent work of the merger to outside storage.
//
// Iterations when the stack height is > 1 are implementing using a
// simple N-way heap merge.
//
// In this design, stacks are treated as immutable via a copy-on-write
// approach whenever a stack is "modified".  So, readers and writers
// essentially don't block each other, and taking a Snapshot is also a
// similarly cheap operation by cloning a stack.

package moss

import (
	"errors"
	"sync"
)

var ErrAllocTooLarge = errors.New("alloc-too-large")
var ErrIteratorDone = errors.New("iterator-done")
var ErrMergeOperatorNil = errors.New("merge-operator-nil")
var ErrMergeOperatorFullMergeFailed = errors.New("merge-operator-full-merge-failed")
var ErrUnimplemented = errors.New("unimplemented")

// A Collection represents an ordered mapping of key-val entries,
// where a Collection is snapshot'able and atomically updatable.
type Collection interface {
	// Start kicks off required background tasks.
	Start() error

	// Close synchronously stops background tasks and releases resources.
	Close() error

	// Snapshot returns a stable Snapshot of the key-value entries.
	Snapshot() (Snapshot, error)

	// NewBatch returns a new Batch instance with preallocated
	// resources.  See the Batch.Alloc() method.
	NewBatch(totalOps, totalKeyValBytes int) (Batch, error)

	// ExecuteBatch atomically incorporates the provided Batch into
	// the Collection.  The Batch instance should not be reused after
	// ExecuteBatch() returns.
	ExecuteBatch(b Batch) error

	// Options returns the options currently being used.
	Options() CollectionOptions
}

// CollectionOptions allows applications to specify config settings.
type CollectionOptions struct {
	// MergeOperator is an optional func provided by an application
	// that wants to use the Merge()'ing feature.
	MergeOperator MergeOperator

	// MinMergePercentage allows the merger to avoid premature merging
	// of segments that are too small, where a segment X has to reach
	// a certain size percentage compared to the next lower segment
	// before segment X (and all segments above X) will be N-way
	// merged downards.
	MinMergePercentage float64

	// MaxStackOpenHeight is the max height of the stack of
	// to-be-merged segments before blocking mutations to allow the
	// merger to catch up.
	MaxStackOpenHeight int

	Debug int // Higher means more logging, when Log != nil.

	Log func(format string, a ...interface{}) // Optional, may be nil.
}

// DefaultCollectionOptions are the default config settings.
var DefaultCollectionOptions = CollectionOptions{
	MergeOperator:      nil,
	MinMergePercentage: 0.8,
	MaxStackOpenHeight: 10,
	Debug:              0,
	Log:                nil,
}

// A Batch is a set of mutations that will be incorporated atomically
// into a Collection.
type Batch interface {
	// Close must be invoked to release resources.
	Close() error

	// Set creates or updates an key-val entry in the Collection.  The
	// key must be unique (not repeated) within the Batch.  Set copies
	// the key and val bytes into the Batch, so the key-val memory may
	// be reused by the caller.
	Set(key, val []byte) error

	// Del deletes a key-val entry from the Collection.  The key must
	// be unique (not repeated) within the Batch.  Del copies the key
	// bytes into the Batch, so the key bytes may be memory by the
	// caller.  Del() on a non-existent key results in a nil error.
	Del(key []byte) error

	// Merge creates or updates a key-val entry in the Collection via
	// the MergeOperator defined in the CollectionOptions.  The key
	// must be unique (not repeated) within the Batch.  Set copies the
	// key and val bytes into the Batch, so the key-val memory may be
	// reused by the caller.
	Merge(key, val []byte) error

	// ----------------------------------------------------

	// Alloc provides a slice of bytes "owned" by the Batch, to reduce
	// extra copying of memory.  See the Collection.NewBatch() method.
	Alloc(numBytes int) ([]byte, error)

	// AllocSet is like Set(), but the caller must provide []byte
	// parameters that came from Alloc().
	AllocSet(keyFromAlloc, valFromAlloc []byte) error

	// AllocDel is like Del(), but the caller must provide []byte
	// parameters that came from Alloc().
	AllocDel(keyFromAlloc []byte) error

	// AllocMerge is like Merge(), but the caller must provide []byte
	// parameters that came from Alloc().
	AllocMerge(keyFromAlloc, valFromAlloc []byte) error
}

// A Snapshot is a stable view of a Collection for readers, isolated
// from concurrent mutation activity.
type Snapshot interface {
	// Close must be invoked to release resources.
	Close() error

	// Get retrieves a val from the Snapshot, and will return nil val
	// if the entry does not exist in the Snapshot.
	Get(key []byte) ([]byte, error)

	// StartIterator returns a new Iterator instance on this Snapshot.
	//
	// On success, the returned Iterator will be positioned so that
	// Iterator.Current() will either provide the first entry in the
	// range or ErrIteratorDone.
	//
	// A startKeyInclusive of nil means the logical "bottom-most"
	// possible key and an endKeyExclusive of nil means the logical
	// "top-most" possible key.
	StartIterator(startKeyInclusive, endKeyExclusive []byte) (Iterator, error)
}

// An Iterator allows enumeration of key-val entries from a Snapshot.
type Iterator interface {
	// Close must be invoked to release resources.
	Close() error

	// Next moves the Iterator to the next key-val entry and will
	// return ErrIteratorDone if the Iterator is done.
	Next() error

	// Current returns ErrIteratorDone when the Iterator is done.
	// Otherwise, Current() returns the current key and val, which
	// should be treated as immutable and as "owned" by the Iterator.
	// The key and val bytes will remain available until the next call
	// to Next() or Close().
	Current() (key, val []byte, err error)
}

// A MergeOperator is implemented by applications that wish to use the
// merge functionality.
type MergeOperator interface {
	// Name returns an identifier for this merge operator, which might
	// be used for logging / debugging.
	Name() string

	// FullMerge the full sequence of operands on top of an
	// existingValue and returns the merged value.  The existingValue
	// may be nil if no value currently exists.  If full merge cannot
	// be done, return (nil, false).
	FullMerge(key, existingValue []byte, operands [][]byte) ([]byte, bool)

	// Partially merge two operands.  If partial merge cannot be done,
	// return (nil, false), which will defer processing until a later
	// FullMerge().
	PartialMerge(key, leftOperand, rightOperand []byte) ([]byte, bool)
}

// ------------------------------------------------------------

// NewCollection returns a new, unstarted Collection instance.
func NewCollection(options CollectionOptions) (
	Collection, error) {
	c := &collection{
		options:         options,
		stopCh:          make(chan struct{}),
		pingMergerCh:    make(chan ping, 10),
		doneMergerCh:    make(chan struct{}),
		donePersisterCh: make(chan struct{}),

		awakePersisterCh: make(chan *segmentStack, 10),
	}

	c.stackOpenCond = sync.NewCond(&c.m)

	return c, nil
}
