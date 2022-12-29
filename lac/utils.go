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
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"unsafe"
)

var (
	boolPtrType = reflect.TypeOf((*bool)(nil))
	intPtrType  = reflect.TypeOf((*int)(nil))
	i32PtrType  = reflect.TypeOf((*int32)(nil))
	u32PtrType  = reflect.TypeOf((*uint32)(nil))
	i64PtrType  = reflect.TypeOf((*int64)(nil))
	u64PtrType  = reflect.TypeOf((*uint64)(nil))
	f32PtrType  = reflect.TypeOf((*float32)(nil))
	f64PtrType  = reflect.TypeOf((*float64)(nil))
	strPtrType  = reflect.TypeOf((*string)(nil))
)

const PtrSize = int(unsafe.Sizeof(uintptr(0)))

type emptyInterface struct {
	Type unsafe.Pointer
	Data unsafe.Pointer
}

type reflectedValue struct {
	Type unsafe.Pointer
	Ptr  unsafe.Pointer
}

//go:linkname memclrNoHeapPointers reflect.memclrNoHeapPointers
func memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr)

//go:linkname memmove reflect.memmove
func memmove(to, from unsafe.Pointer, n uintptr)

// noEscape is to cheat the escape analyser to avoid heap alloc.
//
//go:noinline
//go:nosplit
func noEscape(p interface{}) (ret interface{}) {
	*(*[2]uintptr)(unsafe.Pointer(&ret)) = *(*[2]uintptr)(unsafe.Pointer(&p))
	return
}

func data(i interface{}) unsafe.Pointer {
	return (*emptyInterface)(unsafe.Pointer(&i)).Data
}

func interfaceOfUnexported(v reflect.Value) (ret interface{}) {
	v2 := (*reflectedValue)(unsafe.Pointer(&v))
	r := (*emptyInterface)(unsafe.Pointer(&ret))
	r.Type = v2.Type
	r.Data = v2.Ptr
	return
}

func noMalloc(f func()) {
	checkMalloc(0, f)
}

func checkMalloc(max uint64, f func()) {
	var s, e runtime.MemStats
	runtime.ReadMemStats(&s)
	f()
	runtime.ReadMemStats(&e)
	if n := e.Mallocs - s.Mallocs; n > max {
		panic(fmt.Errorf("has %v malloc", n))
	}
}

//============================================================================
// syncPool
//============================================================================

type syncPool[T any] struct {
	sync.Mutex
	New  func() *T
	pool []*T
}

func (p *syncPool[T]) get() *T {
	p.Lock()
	defer p.Unlock()
	if len(p.pool) == 0 {
		return p.New()
	}
	r := p.pool[len(p.pool)-1]
	p.pool = p.pool[:len(p.pool)-1]
	return r
}

func (p *syncPool[T]) put(v *T) {
	p.Lock()
	defer p.Unlock()
	p.pool = append(p.pool, v)
}

func (p *syncPool[T]) clear() {
	p.Lock()
	defer p.Unlock()
	p.pool = nil
}

func (p *syncPool[T]) reserve(cnt int) {
	p.Lock()
	defer p.Unlock()
	for i := 0; i < cnt; i++ {
		p.pool = append(p.pool, p.New())
	}
}