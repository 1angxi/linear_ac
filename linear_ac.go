//
// Copyright (C) 2020-2021 crazybie@github.com.
//
//
// Linear Allocator
//
// Improve the memory allocation and garbage collection performance.
//
// https://github.com/crazybie/linear_ac
//

package linear_ac

import (
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

var (
	DbgMode            = false
	DbgDisableLinearAc = false
	ChunkSize          = 1024 * 8
)

type sliceHeader struct {
	Data unsafe.Pointer
	Len  int
	Cap  int
}

type stringHeader struct {
	Data unsafe.Pointer
	Len  int
}

type emptyInterface struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

//go:linkname reflect_typedmemmove reflect.typedmemmove
func reflect_typedmemmove(typ, dst, src unsafe.Pointer)

var (
	uintptrSize = unsafe.Sizeof(uintptr(0))

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

// GoroutineId

// https://notes.volution.ro/v1/2019/08/notes/23e3644e/

var goRoutineIdOffset uint64 = 0

func goRoutinePtr() uint64

func goRoutineId() uint64 {
	data := (*[32]uint64)(unsafe.Pointer(uintptr(goRoutinePtr())))
	if offset := atomic.LoadUint64(&goRoutineIdOffset); offset != 0 {
		return data[int(offset)]
	}
	id := goRoutineIdSlow()
	matchedCount := 0
	matchedOffset := 0
	for idx, v := range data[:] {
		if v == id {
			matchedOffset = idx
			matchedCount++
			if matchedCount >= 2 {
				break
			}
		}
	}
	if matchedCount == 1 {
		atomic.StoreUint64(&goRoutineIdOffset, uint64(matchedOffset))
	}
	return id
}

func goRoutineIdSlow() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	stk := strings.TrimPrefix(string(buf[:n]), "goroutine ")
	idField := strings.Fields(stk)[0]
	if id, err := strconv.Atoi(idField); err != nil {
		panic(fmt.Errorf("invalid goroutine id: %v, err: %v", idField, err))
	} else {
		return uint64(id)
	}
}

// Pool

type Pool struct {
	sync.Mutex
	New  func() interface{}
	pool []interface{}
}

func (p *Pool) Get() interface{} {
	p.Lock()
	defer p.Unlock()
	if len(p.pool) == 0 {
		return p.New()
	}
	r := p.pool[len(p.pool)-1]
	p.pool = p.pool[:len(p.pool)-1]
	return r
}

func (p *Pool) Put(v interface{}) {
	p.Lock()
	defer p.Unlock()
	p.pool = append(p.pool, v)
}

// Chunk

type chunk []byte

var chunkPool = Pool{
	New: func() interface{} {
		ck := make(chunk, 0, ChunkSize)
		return &ck
	},
}

// Objects in sync.Pool will be recycled on demand by the system (usually after two full-gc).
// we can put chunks here to make pointers live longer,
// useful to diagnosis use-after-free bugs.
var diagnosisChunkPool = sync.Pool{}

// Allocator

type Allocator struct {
	disabled bool
	chunks   []*chunk
	curChunk int
	scanObjs []interface{}
	maps     map[unsafe.Pointer]struct{}
}

// buildInAc switches to native allocator.
var buildInAc = &Allocator{disabled: true}

var acPool = Pool{
	New: func() interface{} {
		return NewLinearAc()
	},
}

func NewLinearAc() *Allocator {
	ac := &Allocator{
		disabled: DbgDisableLinearAc,
	}
	return ac
}

// Bind allocator to goroutine

var acMap = sync.Map{}

func BindNew() *Allocator {
	ac := acPool.Get().(*Allocator)
	acMap.Store(goRoutineId(), ac)
	return ac
}

func Get() *Allocator {
	if val, ok := acMap.Load(goRoutineId()); ok {
		return val.(*Allocator)
	}
	return buildInAc
}

func (ac *Allocator) Unbind() {
	if Get() == ac {
		acMap.Delete(goRoutineId())
	}
}

func (ac *Allocator) Release() {
	if ac == buildInAc {
		return
	}
	ac.Unbind()
	ac.reset()
	acPool.Put(ac)
}

func (ac *Allocator) reset() {
	if ac.disabled {
		return
	}

	if DbgMode {
		ac.debugCheck()
		ac.scanObjs = ac.scanObjs[:0]
	}

	for _, ck := range ac.chunks {
		*ck = (*ck)[:0]
		if DbgMode {
			diagnosisChunkPool.Put(ck)
		} else {
			chunkPool.Put(ck)
		}
	}
	// clear all ref
	ac.chunks = ac.chunks[:cap(ac.chunks)]
	for i := 0; i < cap(ac.chunks); i++ {
		ac.chunks[i] = nil
	}
	ac.chunks = ac.chunks[:0]
	ac.curChunk = 0

	for k := range ac.maps {
		delete(ac.maps, k)
	}

	ac.disabled = DbgDisableLinearAc
}

func noEscape(p interface{}) (ret interface{}) {
	r := *(*[2]uintptr)(unsafe.Pointer(&p))
	*(*[2]uintptr)(unsafe.Pointer(&ret)) = r
	return
}

func (ac *Allocator) New(ptrToPtr interface{}) {
	tmp := noEscape(ptrToPtr)

	if ac.disabled {
		tp := reflect.TypeOf(tmp).Elem().Elem()
		reflect.ValueOf(tmp).Elem().Set(reflect.New(tp))
		return
	}

	tp := reflect.TypeOf(tmp).Elem()
	v := ac.typedNew(tp, true)
	*(*uintptr)((*emptyInterface)(unsafe.Pointer(&tmp)).data) = (uintptr)((*emptyInterface)(unsafe.Pointer(&v)).data)
}

// NewCopy is useful for code migration.
// native mode is slower than new() due to the additional memory move from stack to heap,
// this is on purpose to avoid heap alloc in linear mode.
func (ac *Allocator) NewCopy(ptr interface{}) (ret interface{}) {
	ptrTemp := noEscape(ptr)
	ptrType := reflect.TypeOf(ptrTemp)
	tp := ptrType.Elem()

	if ac.disabled {
		ret = reflect.New(tp).Interface()
		src := (*emptyInterface)(unsafe.Pointer(&ptrTemp)).data
		dst := (*emptyInterface)(unsafe.Pointer(&ret)).data
		reflect_typedmemmove((*emptyInterface)(unsafe.Pointer(&tp)).data, dst, src)
	} else {
		ret = ac.typedNew(ptrType, false)
		copyBytes((*emptyInterface)(unsafe.Pointer(&ptrTemp)).data, (*emptyInterface)(unsafe.Pointer(&ret)).data, int(tp.Size()))
	}

	return
}

func (ac *Allocator) typedNew(ptrTp reflect.Type, zero bool) (ret interface{}) {
	objType := ptrTp.Elem()
	ptr := ac.alloc(int(objType.Size()), zero)
	retEface := (*emptyInterface)(unsafe.Pointer(&ret))
	retEface.typ = (*emptyInterface)(unsafe.Pointer(&ptrTp)).data
	retEface.data = ptr

	if DbgMode {
		if objType.Kind() == reflect.Struct {
			ac.scanObjs = append(ac.scanObjs, ret)
		}
	}
	return
}

func (ac *Allocator) alloc(need int, zero bool) unsafe.Pointer {
	if len(ac.chunks) == 0 {
		ac.chunks = append(ac.chunks, chunkPool.Get().(*chunk))
	}
start:
	cur := ac.chunks[ac.curChunk]
	used := len(*cur)
	if used+need > cap(*cur) {
		if ac.curChunk == len(ac.chunks)-1 {
			var ck *chunk
			if need > ChunkSize {
				c := make(chunk, 0, need)
				ck = &c
			} else {
				ck = chunkPool.Get().(*chunk)
			}
			ac.chunks = append(ac.chunks, ck)
		} else if cap(*ac.chunks[ac.curChunk+1]) < need {
			chunkPool.Put(ac.chunks[ac.curChunk+1])
			ck := make(chunk, 0, need)
			ac.chunks[ac.curChunk+1] = &ck
		}
		ac.curChunk++
		goto start
	}
	*cur = (*cur)[:used+need]
	ptr := unsafe.Pointer((uintptr)((*sliceHeader)(unsafe.Pointer(cur)).Data) + uintptr(used))
	if zero {
		clearBytes(ptr, need)
	}
	return ptr
}

func copyBytes(src, dst unsafe.Pointer, len int) {
	alignedEnd := uintptr(len) / uintptrSize * uintptrSize
	i := uintptr(0)
	for ; i < alignedEnd; i += uintptrSize {
		*(*uintptr)(unsafe.Pointer(uintptr(dst) + i)) = *(*uintptr)(unsafe.Pointer(uintptr(src) + i))
	}
	for ; i < uintptr(len); i++ {
		*(*byte)(unsafe.Pointer(uintptr(dst) + i)) = *(*byte)(unsafe.Pointer(uintptr(src) + i))
	}
}

func clearBytes(dst unsafe.Pointer, len int) {
	alignedEnd := uintptr(len) / uintptrSize * uintptrSize
	i := uintptr(0)
	for ; i < alignedEnd; i += uintptrSize {
		*(*uintptr)(unsafe.Pointer(uintptr(dst) + i)) = 0
	}
	for ; i < uintptr(len); i++ {
		*(*byte)(unsafe.Pointer(uintptr(dst) + i)) = 0
	}
}

func (ac *Allocator) NewString(v string) string {
	if ac.disabled {
		return v
	}
	h := (*stringHeader)(unsafe.Pointer(&v))
	ptr := ac.alloc(h.Len, false)
	copyBytes(h.Data, ptr, h.Len)
	h.Data = ptr
	return v
}

// NewMap use build-in allocator
func (ac *Allocator) NewMap(mapPtr interface{}) {
	mapPtrTemp := noEscape(mapPtr)

	if ac.disabled {
		tp := reflect.TypeOf(mapPtrTemp).Elem()
		reflect.ValueOf(mapPtrTemp).Elem().Set(reflect.MakeMap(tp))
		return
	}

	m := reflect.MakeMap(reflect.TypeOf(mapPtrTemp).Elem())
	i := m.Interface()
	v := (*emptyInterface)(unsafe.Pointer(&i))
	reflect.ValueOf(mapPtrTemp).Elem().Set(m)

	if ac.maps == nil {
		ac.maps = make(map[unsafe.Pointer]struct{})
	}
	ac.maps[v.data] = struct{}{}
}

func (ac *Allocator) NewSlice(slicePtr interface{}, len, cap_ int) {
	slicePtrTmp := noEscape(slicePtr)

	if ac.disabled {
		v := reflect.MakeSlice(reflect.TypeOf(slicePtrTmp).Elem(), len, cap_)
		reflect.ValueOf(slicePtrTmp).Elem().Set(v)
		return
	}

	refSlicePtrType := reflect.TypeOf(slicePtrTmp)
	if refSlicePtrType.Kind() != reflect.Ptr || refSlicePtrType.Elem().Kind() != reflect.Slice {
		panic(fmt.Errorf("need a pointer to slice"))
	}

	if cap_ < len {
		cap_ = len
	}

	sliceEface := (*emptyInterface)(unsafe.Pointer(&slicePtrTmp))
	slice_ := (*sliceHeader)(sliceEface.data)
	slice_.Data = ac.alloc(cap_*int(refSlicePtrType.Elem().Elem().Size()), false)
	slice_.Len = len
	slice_.Cap = cap_
}

// Useful to create simple slice (simple type as element)
func (ac *Allocator) CopySlice(slice interface{}) (ret interface{}) {
	sliceTmp := noEscape(slice)
	if ac.disabled {
		return sliceTmp
	}

	sliceType := reflect.TypeOf(sliceTmp)
	elemType := sliceType.Elem()
	if sliceType.Kind() != reflect.Slice {
		panic(fmt.Errorf("need a slice"))
	}
	switch elemType.Kind() {
	case reflect.Int, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
	default:
		panic(fmt.Errorf("must not be scalar type"))
	}

	// input is a temp copy, directly use it.

	ret = sliceTmp
	sliceEface := (*emptyInterface)(unsafe.Pointer(&sliceTmp))
	sliceHeader_ := (*sliceHeader)(sliceEface.data)
	need := int(elemType.Size()) * sliceHeader_.Len
	dst := ac.alloc(need, false)
	copyBytes(sliceHeader_.Data, dst, need)
	sliceHeader_.Data = dst

	runtime.KeepAlive(slice)
	return ret
}

func (ac *Allocator) SliceAppend(slicePtr interface{}, elem interface{}) {
	slicePtrTmp := noEscape(slicePtr)

	if ac.disabled {
		s := reflect.ValueOf(slicePtrTmp).Elem()
		v := reflect.Append(s, reflect.ValueOf(elem))
		s.Set(v)
		return
	}

	refSlicePtrTp := reflect.TypeOf(slicePtrTmp)
	if refSlicePtrTp.Kind() != reflect.Ptr || refSlicePtrTp.Elem().Kind() != reflect.Slice {
		panic(fmt.Errorf("expect pointer to slice"))
	}
	refInputTp := reflect.TypeOf(elem)
	refElemTp := refSlicePtrTp.Elem().Elem()
	if refElemTp != refInputTp && elem != nil {
		panic(fmt.Errorf("elem type not match with slice"))
	}

	sliceEface := (*emptyInterface)(unsafe.Pointer(&slicePtrTmp))
	slice_ := (*sliceHeader)(sliceEface.data)
	elemEface := (*emptyInterface)(unsafe.Pointer(&elem))
	elemSz := int(refElemTp.Size())

	// grow
	if slice_.Len >= slice_.Cap {
		pre := *slice_
		if slice_.Cap >= 16 {
			slice_.Cap = int(float32(slice_.Cap) * 1.5)
		} else {
			slice_.Cap *= 2
		}
		if slice_.Cap == 0 {
			slice_.Cap = 1
		}
		slice_.Data = ac.alloc(slice_.Cap*elemSz, false)
		copyBytes(pre.Data, slice_.Data, pre.Len*elemSz)
		slice_.Len = pre.Len
	}

	// append
	if slice_.Len < slice_.Cap {
		d := unsafe.Pointer(uintptr(slice_.Data) + uintptr(elemSz)*uintptr(slice_.Len))
		if refElemTp.Kind() == reflect.Ptr {
			*(*uintptr)(d) = (uintptr)(elemEface.data)
		} else {
			copyBytes(elemEface.data, d, elemSz)
		}
		slice_.Len++
	}
}

func (ac *Allocator) Enum(e interface{}) interface{} {
	temp := noEscape(e)
	if ac.disabled {
		r := reflect.New(reflect.TypeOf(temp))
		r.Elem().Set(reflect.ValueOf(temp))
		return r.Interface()
	}
	tp := reflect.TypeOf(temp)
	r := ac.typedNew(reflect.PtrTo(tp), false)
	copyBytes((*emptyInterface)(unsafe.Pointer(&temp)).data, (*emptyInterface)(unsafe.Pointer(&r)).data, int(tp.Size()))
	return r
}

// Use 1 instead of nil or MaxUint64 to
// 1. make non-nil check pass.
// 2. generate a recoverable panic.
const trickyAddress = uintptr(1)

func (ac *Allocator) internalPointer(addr uintptr) bool {
	if addr == 0 || addr == trickyAddress {
		return true
	}
	for _, c := range ac.chunks {
		h := (*sliceHeader)(unsafe.Pointer(c))
		if addr >= uintptr(h.Data) && addr < uintptr(h.Data)+uintptr(h.Cap) {
			return true
		}
	}
	return false
}

// NOTE: all memories must be referenced by structs.
func (ac *Allocator) debugCheck() {
	checked := map[interface{}]struct{}{}
	// reverse order to bypass obfuscated pointers
	for i := len(ac.scanObjs) - 1; i >= 0; i-- {
		ptr := ac.scanObjs[i]
		if _, ok := checked[ptr]; ok {
			continue
		}
		if err := ac.checkRecursively(reflect.ValueOf(ptr), checked); err != nil {
			panic(err)
		}
	}
}

func (ac *Allocator) checkRecursively(pe reflect.Value, checked map[interface{}]struct{}) error {
	defer func() {
		if err := recover(); err != nil {
			panic(err)
		}
	}()

	if pe.Kind() == reflect.Ptr {
		if pe.Pointer() != trickyAddress && !pe.IsNil() {
			if !ac.internalPointer(pe.Pointer()) {
				return fmt.Errorf("unexpected external pointer: %+v", pe)
			}
			if pe.Elem().Type().Kind() == reflect.Struct {
				if err := ac.checkRecursively(pe.Elem(), checked); err != nil {
					return err
				}
				checked[pe.Interface()] = struct{}{}
			}
		}
		return nil
	}

	tp := pe.Type()
	fieldName := func(i int) string {
		return fmt.Sprintf("%v.%v", tp.Name(), tp.Field(i).Name)
	}

	if pe.Kind() == reflect.Struct {
		for i := 0; i < pe.NumField(); i++ {
			f := pe.Field(i)

			switch f.Kind() {
			case reflect.Ptr:
				if err := ac.checkRecursively(f, checked); err != nil {
					return fmt.Errorf("%v: %v", fieldName(i), err)
				}
				*(*uintptr)(unsafe.Pointer(f.UnsafeAddr())) = trickyAddress

			case reflect.Slice:
				h := (*sliceHeader)(unsafe.Pointer(f.UnsafeAddr()))
				if f.Len() > 0 && h.Data != nil {
					if !ac.internalPointer((uintptr)(h.Data)) {
						return fmt.Errorf("%s: unexpected external slice: %s", fieldName(i), f.String())
					}
					for j := 0; j < f.Len(); j++ {
						if err := ac.checkRecursively(f.Index(j), checked); err != nil {
							return fmt.Errorf("%v: %v", fieldName(i), err)
						}
					}
				}
				h.Data = nil
				h.Len = math.MaxInt32
				h.Cap = math.MaxInt32

			case reflect.Array:
				for j := 0; j < f.Len(); j++ {
					if err := ac.checkRecursively(f.Index(j), checked); err != nil {
						return fmt.Errorf("%v: %v", fieldName(i), err)
					}
				}

			case reflect.Map:
				m := *(*unsafe.Pointer)(unsafe.Pointer(f.UnsafeAddr()))
				if _, ok := ac.maps[m]; !ok {
					return fmt.Errorf("%v: unexpected external map: %+v", fieldName(i), f)
				}
				for iter := f.MapRange(); iter.Next(); {
					if err := ac.checkRecursively(iter.Value(), checked); err != nil {
						return fmt.Errorf("%v: %v", fieldName(i), err)
					}
				}
			}
		}
	}
	return nil
}

func (ac *Allocator) Bool(v bool) (r *bool) {
	if ac.disabled {
		r = new(bool)
	} else {
		r = ac.typedNew(boolPtrType, false).(*bool)
	}
	*r = v
	return
}

func (ac *Allocator) Int(v int) (r *int) {
	if ac.disabled {
		r = new(int)
	} else {
		r = ac.typedNew(intPtrType, false).(*int)
	}
	*r = v
	return
}

func (ac *Allocator) Int32(v int32) (r *int32) {
	if ac.disabled {
		r = new(int32)
	} else {
		r = ac.typedNew(i32PtrType, false).(*int32)
	}
	*r = v
	return
}

func (ac *Allocator) Uint32(v uint32) (r *uint32) {
	if ac.disabled {
		r = new(uint32)
	} else {
		r = ac.typedNew(u32PtrType, false).(*uint32)
	}
	*r = v
	return
}

func (ac *Allocator) Int64(v int64) (r *int64) {
	if ac.disabled {
		r = new(int64)
	} else {
		r = ac.typedNew(i64PtrType, false).(*int64)
	}
	*r = v
	return
}

func (ac *Allocator) Uint64(v uint64) (r *uint64) {
	if ac.disabled {
		r = new(uint64)
	} else {
		r = ac.typedNew(u64PtrType, false).(*uint64)
	}
	*r = v
	return
}

func (ac *Allocator) Float32(v float32) (r *float32) {
	if ac.disabled {
		r = new(float32)
	} else {
		r = ac.typedNew(f32PtrType, false).(*float32)
	}
	*r = v
	return
}

func (ac *Allocator) Float64(v float64) (r *float64) {
	if ac.disabled {
		r = new(float64)
	} else {
		r = ac.typedNew(f64PtrType, false).(*float64)
	}
	*r = v
	return
}

func (ac *Allocator) String(v string) (r *string) {
	if ac.disabled {
		r = new(string)
		*r = v
	} else {
		r = ac.typedNew(strPtrType, false).(*string)
		*r = ac.NewString(v)
	}
	return
}
