// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

type slice struct {
	array unsafe.Pointer  // 指向底层数组的指针
	len   int  // slice的大小
	cap   int  // slice的容量
}

// A notInHeapSlice is a slice backed by go:notinheap memory.
type notInHeapSlice struct {
	array *notInHeap
	len   int
	cap   int
}

func panicmakeslicelen() {
	panic(errorString("makeslice: len out of range"))
}

func panicmakeslicecap() {
	panic(errorString("makeslice: cap out of range"))
}

func makeslice(et *_type, len, cap int) unsafe.Pointer {  // make([]int) 会被翻译成这个函数
	mem, overflow := math.MulUintptr(et.size, uintptr(cap))  // 判断容量是否太大导致地址溢出
	if overflow || mem > maxAlloc || len < 0 || len > cap {  // 如果溢出或者需要的内存大于最大能分配内存或大小<0或者大小比容量还大，则panic
		// NOTE: Produce a 'len out of range' error instead of a
		// 'cap out of range' error when someone does make([]T, bignumber).
		// 'cap out of range' is true too, but since the cap is only being
		// supplied implicitly, saying len is clearer.
		// See golang.org/issue/4085.
		mem, overflow := math.MulUintptr(et.size, uintptr(len))
		if overflow || mem > maxAlloc || len < 0 {
			panicmakeslicelen()
		}
		panicmakeslicecap()
	}

	return mallocgc(mem, et, true)  // 从堆中分配底层数组，返回指向数组开始位置的指针
}

func makeslice64(et *_type, len64, cap64 int64) unsafe.Pointer {
	len := int(len64)
	if int64(len) != len64 {
		panicmakeslicelen()
	}

	cap := int(cap64)
	if int64(cap) != cap64 {
		panicmakeslicecap()
	}

	return makeslice(et, len, cap)
}

// growslice handles slice growth during append.
// It is passed the slice element type, the old slice, and the desired new minimum capacity,
// and it returns a new slice with at least that capacity, with the old data
// copied into it.
// The new slice's length is set to the old slice's length,
// NOT to the new requested capacity.
// This is for codegen convenience. The old slice's length is used immediately
// to calculate where to write new values during an append.
// TODO: When the old backend is gone, reconsider this decision.
// The SSA backend might prefer the new length or to return only ptr/cap and save stack space.
func growslice(et *_type, old slice, cap int) slice {  // 当append发现需要扩容时会调用这个方法。cap是指slice的大小加append的元素的个数后的值，表示大小至少要这么多
	if raceenabled {
		callerpc := getcallerpc()
		racereadrangepc(old.array, uintptr(old.len*int(et.size)), callerpc, funcPC(growslice))
	}
	if msanenabled {
		msanread(old.array, uintptr(old.len*int(et.size)))
	}

	if cap < old.cap {  // 虽然append有判断，但这里double check
		panic(errorString("growslice: cap out of range"))
	}

	if et.size == 0 {  // 如果元素类型的大小是0，则返回一个slice，它的底层数组指针指向一个0内存分配的位置
		// append should not create a slice with nil pointer but non-zero len.
		// We assume that append doesn't need to preserve old.array in this case.
		return slice{unsafe.Pointer(&zerobase), old.len, cap}
	}

	newcap := old.cap
	doublecap := newcap + newcap  // 原来的容量翻倍
	if cap > doublecap {  // 如果需要的大于两倍的原来容量，则新容量选择需要的容量。意味着append后所有容量都将被占用，大小等于容量，如果这个slice频繁的append，则会一直扩容，降低性能
		newcap = cap
	} else {  // 如果两倍的后足够存放数据了
		if old.len < 1024 {  // 如果append前的元素个数小于1024，则新容量就设置为双倍
			newcap = doublecap
		} else {  // 如果原来的元素个数比1024还大，则不采用翻倍，而是0.25被增长，直到足够。这种做法不至于浪费内存，但是如果这个slice频繁的append，则会一直扩容，降低性能
			// Check 0 < newcap to detect overflow
			// and prevent an infinite loop.
			for 0 < newcap && newcap < cap {  // 从老容量开始0.25倍增长，直到新容量大于需要的容量
				newcap += newcap / 4
			}
			// Set newcap to the requested cap when
			// the newcap calculation overflowed.
			if newcap <= 0 {
				newcap = cap
			}
		}
	}
	// 新容量选定后，开始扩容
	var overflow bool
	var lenmem, newlenmem, capmem uintptr
	// Specialize for common values of et.size.
	// For 1 we don't need any division/multiplication.
	// For sys.PtrSize, compiler will optimize division/multiplication into a shift by a constant.
	// For powers of 2, use a variable shift.
	switch {
	case et.size == 1:  // 如果元素类型占用大小为1
		lenmem = uintptr(old.len)
		newlenmem = uintptr(cap)
		capmem = roundupsize(uintptr(newcap))
		overflow = uintptr(newcap) > maxAlloc
		newcap = int(capmem)
	case et.size == sys.PtrSize:
		lenmem = uintptr(old.len) * sys.PtrSize  // 得到append前所有数据占用的空间大小
		newlenmem = uintptr(cap) * sys.PtrSize  // 得到append后所有数据占用的空间大小
		capmem = roundupsize(uintptr(newcap) * sys.PtrSize)  // 得到新容量占用的空间大小。如果不是8kb的倍数，则向上取，16kb、24kb、32kb
		overflow = uintptr(newcap) > maxAlloc/sys.PtrSize
		newcap = int(capmem / sys.PtrSize)  // 修正新容量
	case isPowerOfTwo(et.size):
		var shift uintptr
		if sys.PtrSize == 8 {
			// Mask shift for better code generation.
			shift = uintptr(sys.Ctz64(uint64(et.size))) & 63
		} else {
			shift = uintptr(sys.Ctz32(uint32(et.size))) & 31
		}
		lenmem = uintptr(old.len) << shift
		newlenmem = uintptr(cap) << shift
		capmem = roundupsize(uintptr(newcap) << shift)
		overflow = uintptr(newcap) > (maxAlloc >> shift)
		newcap = int(capmem >> shift)
	default:
		lenmem = uintptr(old.len) * et.size
		newlenmem = uintptr(cap) * et.size
		capmem, overflow = math.MulUintptr(et.size, uintptr(newcap))
		capmem = roundupsize(capmem)
		newcap = int(capmem / et.size)
	}

	// The check of overflow in addition to capmem > maxAlloc is needed
	// to prevent an overflow which can be used to trigger a segfault
	// on 32bit architectures with this example program:
	//
	// type T [1<<27 + 1]int64
	//
	// var d T
	// var s []T
	//
	// func main() {
	//   s = append(s, d, d, d, d)
	//   print(len(s), "\n")
	// }
	if overflow || capmem > maxAlloc {
		panic(errorString("growslice: cap out of range"))
	}

	var p unsafe.Pointer
	if et.ptrdata == 0 {  // 如果元素是值类型
		p = mallocgc(capmem, nil, false)  // 申请新容量的空间出来
		// The append() that calls growslice is going to overwrite from old.len to cap (which will be the new length).
		// Only clear the part that will not be overwritten.
		memclrNoHeapPointers(add(p, newlenmem), capmem-newlenmem)  // 清除 `新空间开始指针+至少需要的容量（被数据占满）` 的位置后面的数据
	} else {  // 如果元素是指针类型，就需要扫描标记，mallocgc的参数不一样
		// Note: can't use rawmem (which avoids zeroing of memory), because then GC can scan uninitialized memory.
		p = mallocgc(capmem, et, true)
		if lenmem > 0 && writeBarrier.enabled {
			// Only shade the pointers in old.array since we know the destination slice p
			// only contains nil pointers because it has been cleared during alloc.
			bulkBarrierPreWriteSrcOnly(uintptr(p), uintptr(old.array), lenmem)
		}
	}
	memmove(p, old.array, lenmem)  // 将原slice中的数据直接copy到新空间

	return slice{p, old.len, newcap}
}

func isPowerOfTwo(x uintptr) bool {
	return x&(x-1) == 0
}

func slcecopy(to, fm slice, width uintptr) int {
	if fm.len == 0 || to.len == 0 {
		return 0
	}

	n := fm.len
	if to.len < n {
		n = to.len
	}

	if width == 0 {
		return n
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(slicecopy)
		racewriterangepc(to.array, uintptr(n*int(width)), callerpc, pc)
		racereadrangepc(fm.array, uintptr(n*int(width)), callerpc, pc)
	}
	if msanenabled {
		msanwrite(to.array, uintptr(n*int(width)))
		msanread(fm.array, uintptr(n*int(width)))
	}

	size := uintptr(n) * width
	if size == 1 { // common case worth about 2x to do here
		// TODO: is this still worth it with new memmove impl?
		*(*byte)(to.array) = *(*byte)(fm.array) // known to be a byte pointer
	} else {
		memmove(to.array, fm.array, size)
	}
	return n
}

func slicestringcopy(to []byte, fm string) int {
	if len(fm) == 0 || len(to) == 0 {
		return 0
	}

	n := len(fm)
	if len(to) < n {
		n = len(to)
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(slicestringcopy)
		racewriterangepc(unsafe.Pointer(&to[0]), uintptr(n), callerpc, pc)
	}
	if msanenabled {
		msanwrite(unsafe.Pointer(&to[0]), uintptr(n))
	}

	memmove(unsafe.Pointer(&to[0]), stringStructOf(&fm).str, uintptr(n))
	return n
}
