// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package nbs

import (
	"sort"
	"sync"

	"github.com/attic-labs/noms/go/chunks"
	"github.com/attic-labs/noms/go/constants"
	"github.com/attic-labs/noms/go/d"
	"github.com/attic-labs/noms/go/hash"
	"github.com/attic-labs/noms/go/types"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
)

// The root of a Noms Chunk Store is stored in a 'manifest', along with the
// names of the tables that hold all the chunks in the store. The number of
// chunks in each table is also stored in the manifest.

type EnumerationOrder uint8

const (
	// StorageVersion is the version of the on-disk Noms Chunks Store data format.
	StorageVersion = "0"

	defaultMemTableSize uint64 = 512 * 1 << 20 // 512MB
	defaultAWSReadLimit        = 1024

	InsertOrder EnumerationOrder = iota
	ReverseOrder
)

type NomsBlockStore struct {
	mm          manifest
	nomsVersion string

	mu     sync.RWMutex // protects the following state
	mt     *memTable
	mtSize uint64
	tables tableSet
	root   hash.Hash

	putCount uint64
}

type AWSStoreFactory struct {
	sess          *session.Session
	table, bucket string
	indexCache    *s3IndexCache
	readRl        chan struct{}
}

func NewAWSStoreFactory(sess *session.Session, table, bucket string, indexCacheSize uint64) chunks.Factory {
	var indexCache *s3IndexCache
	if indexCacheSize > 0 {
		indexCache = newS3IndexCache(indexCacheSize)
	}
	return &AWSStoreFactory{sess, table, bucket, indexCache, make(chan struct{}, defaultAWSReadLimit)}
}

func (asf *AWSStoreFactory) CreateStore(ns string) chunks.ChunkStore {
	return newAWSStore(asf.table, ns, asf.bucket, asf.sess, defaultMemTableSize, asf.indexCache, asf.readRl)
}

func (asf *AWSStoreFactory) Shutter() {
}

func NewAWSStore(table, ns, bucket string, sess *session.Session, memTableSize uint64) *NomsBlockStore {
	return newAWSStore(table, ns, bucket, sess, memTableSize, nil, nil)
}

func newAWSStore(table, ns, bucket string, sess *session.Session, memTableSize uint64, indexCache *s3IndexCache, readRl chan struct{}) *NomsBlockStore {
	mm := newDynamoManifest(table, ns, dynamodb.New(sess))
	ts := newS3TableSet(s3.New(sess), bucket, indexCache, readRl)
	return newNomsBlockStore(mm, ts, memTableSize)
}

func NewLocalStore(dir string, memTableSize uint64) *NomsBlockStore {
	return newNomsBlockStore(fileManifest{dir}, newFSTableSet(dir), memTableSize)
}

func newNomsBlockStore(mm manifest, ts tableSet, memTableSize uint64) *NomsBlockStore {
	if memTableSize == 0 {
		memTableSize = defaultMemTableSize
	}
	nbs := &NomsBlockStore{
		mm:          mm,
		tables:      ts,
		nomsVersion: constants.NomsVersion,
		mtSize:      memTableSize,
	}

	if exists, vers, root, tableSpecs := nbs.mm.ParseIfExists(nil); exists {
		nbs.nomsVersion, nbs.root = vers, root
		nbs.tables = nbs.tables.Union(tableSpecs)
	}

	return nbs
}

func (nbs *NomsBlockStore) Put(c chunks.Chunk) {
	a := addr(c.Hash())
	d.PanicIfFalse(nbs.addChunk(a, c.Data()))
	nbs.putCount++
}

func (nbs *NomsBlockStore) SchedulePut(c chunks.Chunk, refHeight uint64, hints types.Hints) {
	nbs.Put(c)
}

func (nbs *NomsBlockStore) PutMany(chunx []chunks.Chunk) (err chunks.BackpressureError) {
	for ; len(chunx) > 0; chunx = chunx[1:] {
		c := chunx[0]
		a := addr(c.Hash())
		if !nbs.addChunk(a, c.Data()) {
			break
		}
		nbs.putCount++
	}
	for _, c := range chunx {
		err = append(err, c.Hash())
	}

	return err
}

// TODO: figure out if there's a non-error reason for this to return false. If not, get rid of return value.
func (nbs *NomsBlockStore) addChunk(h addr, data []byte) bool {
	nbs.mu.Lock()
	defer nbs.mu.Unlock()
	if nbs.mt == nil {
		nbs.mt = newMemTable(nbs.mtSize)
	}
	if !nbs.mt.addChunk(h, data) {
		nbs.tables = nbs.tables.Prepend(nbs.mt)
		nbs.mt = newMemTable(nbs.mtSize)
		return nbs.mt.addChunk(h, data)
	}
	return true
}

func (nbs *NomsBlockStore) Get(h hash.Hash) chunks.Chunk {
	a := addr(h)
	data, tables := func() (data []byte, tables chunkReader) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		if nbs.mt != nil {
			data = nbs.mt.get(a)
		}
		return data, nbs.tables
	}()
	if data != nil {
		return chunks.NewChunkWithHash(h, data)
	}
	if data := tables.get(a); data != nil {
		return chunks.NewChunkWithHash(h, data)
	}
	return chunks.EmptyChunk
}

func (nbs *NomsBlockStore) GetMany(hashes []hash.Hash) []chunks.Chunk {
	reqs := toGetRecords(hashes)

	wg := &sync.WaitGroup{}

	tables, remaining := func() (tables chunkReader, remaining bool) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		tables = nbs.tables

		if nbs.mt != nil {
			remaining = nbs.mt.getMany(reqs, &sync.WaitGroup{})
			wg.Wait()
		} else {
			remaining = true
		}

		return
	}()

	sort.Sort(getRecordByPrefix(reqs))

	if remaining {
		tables.getMany(reqs, wg)
		wg.Wait()
	}

	sort.Sort(getRecordByOrder(reqs))

	resp := make([]chunks.Chunk, len(hashes))
	for i, req := range reqs {
		if req.data == nil {
			resp[i] = chunks.EmptyChunk
		} else {
			resp[i] = chunks.NewChunkWithHash(hashes[i], req.data)
		}
	}

	return resp
}

func toGetRecords(hashes []hash.Hash) []getRecord {
	reqs := make([]getRecord, len(hashes))
	for i, h := range hashes {
		a := addr(h)
		reqs[i] = getRecord{
			a:      &a,
			prefix: a.Prefix(),
			order:  i,
		}
	}
	return reqs
}

func (nbs *NomsBlockStore) CalcReads(hashes []hash.Hash, blockSize, maxReadSize, ampThresh uint64) (reads int, split bool) {
	reqs := toGetRecords(hashes)
	tables := func() (tables tableSet) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		tables = nbs.tables

		return
	}()

	sort.Sort(getRecordByPrefix(reqs))

	reads, split, remaining := tables.calcReads(reqs, blockSize, maxReadSize, ampThresh)
	d.Chk.False(remaining)
	return
}

func (nbs *NomsBlockStore) extractChunks(order EnumerationOrder, chunkChan chan<- *chunks.Chunk) {
	ch := make(chan extractRecord, 1)
	go func() {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		// Chunks in nbs.tables were inserted before those in nbs.mt, so extract chunks there _first_ if we're doing InsertOrder...
		if order == InsertOrder {
			nbs.tables.extract(order, ch)
		}
		if nbs.mt != nil {
			nbs.mt.extract(order, ch)
		}
		// ...and do them _second_ if we're doing ReverseOrder
		if order == ReverseOrder {
			nbs.tables.extract(order, ch)
		}

		close(ch)
	}()
	for rec := range ch {
		c := chunks.NewChunkWithHash(hash.Hash(rec.a), rec.data)
		chunkChan <- &c
	}
}

func (nbs *NomsBlockStore) Count() uint32 {
	count, tables := func() (count uint32, tables chunkReader) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		if nbs.mt != nil {
			count = nbs.mt.count()
		}
		return count, nbs.tables
	}()
	return count + tables.count()
}

func (nbs *NomsBlockStore) Has(h hash.Hash) bool {
	a := addr(h)
	has, tables := func() (bool, chunkReader) {
		nbs.mu.RLock()
		defer nbs.mu.RUnlock()
		return nbs.mt != nil && nbs.mt.has(a), nbs.tables
	}()
	return has || tables.has(a)
}

func (nbs *NomsBlockStore) Root() hash.Hash {
	nbs.mu.RLock()
	defer nbs.mu.RUnlock()
	return nbs.root
}

func (nbs *NomsBlockStore) UpdateRoot(current, last hash.Hash) bool {
	nbs.mu.Lock()
	defer nbs.mu.Unlock()
	d.Chk.True(nbs.root == last, "UpdateRoot: last != nbs.Root(); %s != %s", last, nbs.root)

	if nbs.mt != nil && nbs.mt.count() > 0 {
		nbs.tables = nbs.tables.Prepend(nbs.mt)
		nbs.mt = nil
	}

	actual, tableNames := nbs.mm.Update(nbs.tables.ToSpecs(), nbs.root, current, nil)

	if current != actual {
		nbs.root = actual
		nbs.tables = nbs.tables.Union(tableNames)
		return false
	}
	nbs.nomsVersion, nbs.root = constants.NomsVersion, current
	return true
}

func (nbs *NomsBlockStore) Version() string {
	return nbs.nomsVersion
}

func (nbs *NomsBlockStore) Close() (err error) {
	nbs.mu.Lock()
	defer nbs.mu.Unlock()
	return nbs.tables.Close()
}

// types.BatchStore
func (nbs *NomsBlockStore) AddHints(hints types.Hints) {
	// noop
}

func (nbs *NomsBlockStore) Flush() {
	success := nbs.UpdateRoot(nbs.root, nbs.root)
	d.Chk.True(success)
}
