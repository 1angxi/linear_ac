/*
 * Linear Allocator
 *
 * Improve the memory allocation and garbage collection performance.
 *
 * Copyright (C) 2020-2022 crazybie@github.com.
 * https://github.com/crazybie/linear_ac
 */

package lac

import (
	"reflect"
	"sync"
	"unsafe"
)

var (
	DbgMode         = false
	DisableLinearAc = false
	ChunkSize       = 1024 * 8
)

// Chunk

type chunk []byte

var chunkPool = Pool[chunk]{
	New: func() chunk {
		return make(chunk, 0, ChunkSize)
	},
}

// Allocator

type Allocator struct {
	sync.Mutex

	disabled       bool
	chunks         []chunk
	curChunk       int
	refCnt         int32
	externalPtr    []unsafe.Pointer
	externalSlice  []unsafe.Pointer
	externalString []unsafe.Pointer
	externalMap    []interface{}

	dbgScanObjs []interface{}
}

func newLac() *Allocator {
	ac := &Allocator{
		disabled: DisableLinearAc,
		refCnt:   1,
		chunks:   make([]chunk, 0, 1),
	}
	return ac
}

func (ac *Allocator) alloc(need int, zero bool) unsafe.Pointer {
	// shared with other goroutines?
	if ac.refCnt > 1 {
		ac.Lock()
		defer ac.Unlock()
	}

	if len(ac.chunks) == 0 {
		ac.chunks = append(ac.chunks, chunkPool.get())
	}

	aligned := (need + PtrSize + 1) & ^(PtrSize - 1)

start:
	cur := ac.chunks[ac.curChunk]
	used := len(cur)

	if used+aligned > cap(cur) {

		if ac.curChunk == len(ac.chunks)-1 {
			// if we get to the end of the chunk list,
			// we enqueue a new one the end of it.
			var ck chunk
			if aligned > ChunkSize {
				// recreate a large chunk
				ck = make(chunk, 0, aligned)
			} else {
				ck = chunkPool.get()
			}
			ac.chunks = append(ac.chunks, ck)
		} else if cap(ac.chunks[ac.curChunk+1]) < aligned {
			// if the next normal chunk is still under required size,
			// recreate a large one and replace it.
			chunkPool.put(ac.chunks[ac.curChunk+1])
			ac.chunks[ac.curChunk+1] = make(chunk, 0, aligned)
		}

		ac.curChunk++
		goto start
	}

	ac.chunks[ac.curChunk] = cur[:used+aligned]
	ptr := unsafe.Add((*sliceHeader)(unsafe.Pointer(&cur)).Data, used)
	if zero {
		memclrNoHeapPointers(ptr, uintptr(aligned))
	}
	return ptr
}

func (ac *Allocator) reset() {
	if ac.disabled {
		return
	}

	if DbgMode {
		ac.debugCheck(true)
		ac.dbgScanObjs = ac.dbgScanObjs[:0]
	}

	for _, ck := range ac.chunks {
		ck = ck[:0]
		if DbgMode {
			diagnosisChunkPool.Put(ck)
		} else {
			// only reuse the normal chunks,
			// otherwise we may have too many large chunks wasted.
			if cap(ck) == ChunkSize {
				chunkPool.put(ck)
			}
		}
	}
	// clear all ref
	ac.chunks = ac.chunks[:cap(ac.chunks)]
	for i := 0; i < cap(ac.chunks); i++ {
		ac.chunks[i] = nil
	}
	ac.chunks = ac.chunks[:0]
	ac.curChunk = 0

	// clear externals
	ac.externalPtr = ac.externalPtr[:0]
	ac.externalSlice = ac.externalSlice[:0]
	ac.externalMap = ac.externalMap[:0]

	ac.disabled = DisableLinearAc
	ac.refCnt = 1
}

func (ac *Allocator) typedAlloc(ptrTp reflect.Type, sz uintptr, zero bool) (ret interface{}) {
	if sz == 0 {
		sz = ptrTp.Elem().Size()
	}
	ptr := ac.alloc(int(sz), zero)
	*(*emptyInterface)(unsafe.Pointer(&ret)) = emptyInterface{data(ptrTp), ptr}

	if DbgMode {
		if ptrTp.Elem().Kind() == reflect.Struct {
			ac.dbgScanObjs = append(ac.dbgScanObjs, ret)
		}
	}

	return
}

func (ac *Allocator) keepAlive(ptr interface{}) {
	if ac.disabled {
		return
	}

	d := data(ptr)
	if d == nil {
		return
	}

	switch reflect.TypeOf(ptr).Kind() {
	case reflect.Ptr:
		ac.externalPtr = append(ac.externalPtr, d)
	case reflect.Slice:
		ac.externalSlice = append(ac.externalSlice, (*sliceHeader)(d).Data)
	case reflect.String:
		ac.externalString = append(ac.externalSlice, (*stringHeader)(d).Data)
	case reflect.Map:
		ac.externalMap = append(ac.externalMap, d)
	case reflect.Func:
		ac.externalPtr = append(ac.externalPtr, reflect.ValueOf(ptr).UnsafePointer())
	}
}
