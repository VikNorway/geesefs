// Copyright 2015 - 2017 Ka-Hing Cheung
// Copyright 2021 Vitaliy Filippov
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

import (
	. "github.com/kahing/goofys/api/common"
	"github.com/jacobsa/fuse/fuseops"
	"io"
	"runtime"
	"runtime/debug"
	"sync"
	"github.com/shirou/gopsutil/mem"
)

var bufferLog = GetLogger("buffer")

// BufferPool tracks memory used by cache buffers
type BufferPool struct {
	mu   sync.Mutex
	cond *sync.Cond

	curDirtyID uint64

	cur uint64
	curDirty uint64
	max uint64

	requests uint64

	limit uint64

	FreeSomeCleanBuffers func(inode fuseops.InodeID, size uint64) uint64
}

// Several FileBuffers may be slices of the same array,
// but we want to track memory usage, so we have to refcount them...
type BufferPointer struct {
	buf []byte
	refs int
}

type FileBuffer struct {
	offset uint64
	// Unmodified chunks (equal to the current server-side object state) have dirtyID = 0.
	// Every write or split assigns a new unique chunk ID.
	// Flusher tracks IDs that are currently being flushed to the server,
	// which allows to do flush and write in parallel.
	dirtyID uint64
	// Is this chunk already saved to the server as a part of multipart upload?
	flushed bool
	// Data
	buf []byte
	ptr *BufferPointer
}

type MultiReader struct {
	buffers [][]byte
	idx int
	pos int64
	bufPos int
	size int64
}

func NewMultiReader(buffers [][]byte) *MultiReader {
	size := int64(0)
	for i := 0; i < len(buffers); i++ {
		size += int64(len(buffers[i]))
	}
	return &MultiReader{
		buffers: buffers,
		size: size,
	}
}

func (r *MultiReader) Read(buf []byte) (n int, err error) {
	n = 0
	if r.idx >= len(r.buffers) {
		err = io.EOF
		return
	}
	remaining := len(buf)
	for r.idx < len(r.buffers) {
		l := len(r.buffers[r.idx]) - r.bufPos
		if l > remaining {
			l = remaining
		}
		copy(buf[n : n+l], r.buffers[r.idx][r.bufPos : r.bufPos+l])
		n += l
		r.pos = r.pos + int64(l)
		r.bufPos += l
		if r.bufPos >= len(r.buffers[r.idx]) {
			r.idx++
			r.bufPos = 0
		}
	}
	return
}

func (r *MultiReader) Seek(offset int64, whence int) (newOffset int64, err error) {
	if whence == io.SeekEnd {
		offset += r.size
	} else if whence == io.SeekCurrent {
		offset += r.pos
	}
	if offset > r.size {
		offset = r.size
	}
	if offset < 0 {
		offset = 0
	}
	r.idx = 0
	r.pos = 0
	r.bufPos = 0
	for r.pos < offset {
		end := r.pos + int64(len(r.buffers[r.idx]))
		if end <= offset {
			r.pos = end
			r.idx++
		} else {
			r.bufPos = int(offset-r.pos)
			r.pos = offset
		}
	}
	return r.pos, nil
}

func (r *MultiReader) Len() int64 {
	return r.size
}

func maxMemToUse(usedMem uint64) uint64 {
	m, err := mem.VirtualMemory()
	if err != nil {
		panic(err)
	}

	availableMem, err := getCgroupAvailableMem()
	if err != nil {
		log.Debugf("amount of available memory from cgroup is: %v", availableMem/1024/1024)
	}

	if err != nil || availableMem < 0 || availableMem > m.Available {
		availableMem = m.Available
	}

	log.Debugf("amount of available memory: %v", availableMem/1024/1024)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	log.Debugf("amount of allocated memory: %v/%v MB", ms.Sys/1024/1024, ms.Alloc/1024/1024)

	max := availableMem+usedMem
	log.Debugf("using up to %vMB for in-memory buffers (%v MB used)", max/1024/1024, usedMem/1024/1024)

	return max
}

func (pool BufferPool) Init() *BufferPool {
	pool.cond = sync.NewCond(&pool.mu)
	return &pool
}

func NewBufferPool(limit uint64) *BufferPool {
	pool := BufferPool{limit: limit}.Init()
	return pool
}

func (pool *BufferPool) recomputeBufferLimit() {
	pool.max = maxMemToUse(pool.cur)
	if pool.limit > 0 && pool.max > pool.limit {
		pool.max = pool.limit
	}
}

func (pool *BufferPool) Use(inode fuseops.InodeID, size uint64, dirty bool) {
	bufferLog.Debugf("requesting %v", size)

	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.requests++
	if pool.requests >= 16 {
		debug.FreeOSMemory()
		pool.recomputeBufferLimit()
		pool.requests = 0
	}

	for pool.cur+size > pool.max {
		// Try to free clean buffers, then flush dirty buffers
		freed := pool.FreeSomeCleanBuffers(inode, pool.cur+size - pool.max)
		bufferLog.Debugf("Freed %v, now: %v %v %v", freed, pool.cur, size, pool.max)
		if pool.cur+size > pool.max {
			if pool.cur == 0 {
				debug.FreeOSMemory()
				pool.recomputeBufferLimit()
				if pool.cur+size > pool.max {
					// we don't have any in use buffers, and we've made attempts to
					// free memory AND correct our limits, yet we still can't allocate.
					// it's likely that we are simply asking for too much
					log.Errorf("Unable to allocate %d bytes, used %d bytes, limit is %d bytes", size, pool.cur, pool.max)
					panic("OOM")
				} else {
					break
				}
			} else {
				pool.cond.Wait()
			}
		}
	}

	pool.cur += size
	if dirty {
		pool.curDirty += size
	}
}

func (pool *BufferPool) Free(size uint64, dirty bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.FreeUnlocked(size, dirty)
}

func (pool *BufferPool) FreeUnlocked(size uint64, dirty bool) {
	notify := pool.cur+size > pool.max
	pool.cur -= size
	if dirty {
		pool.curDirty -= size
	}
	if notify {
		pool.cond.Broadcast()
	}
}

func (pool *BufferPool) AddDirty(size int64) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.curDirty = uint64(int64(pool.curDirty)+size)
}

func (pool *BufferPool) FreeBuffer(buffers *[]FileBuffer, i int) uint64 {
	buf := &((*buffers)[i])
	freed := uint64(0)
	buf.ptr.refs--
	if buf.ptr.refs == 0 {
		freed = uint64(len(buf.ptr.buf))
		pool.Free(freed, buf.dirtyID != 0)
	}
	*buffers = append((*buffers)[0 : i], (*buffers)[i+1 : ]...)
	return freed
}

func (pool *BufferPool) FreeBufferUnlocked(buffers *[]FileBuffer, i int) uint64 {
	buf := &((*buffers)[i])
	freed := uint64(0)
	buf.ptr.refs--
	if buf.ptr.refs == 0 {
		freed = uint64(len(buf.ptr.buf))
		pool.FreeUnlocked(freed, buf.dirtyID != 0)
	}
	*buffers = append((*buffers)[0 : i], (*buffers)[i+1 : ]...)
	return freed
}
