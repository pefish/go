// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/bytealg"
	"unsafe"
)

// The constant is known to the compiler.
// There is no fundamental theory behind this number.
const tmpStringBufSize = 32

type tmpBuf [tmpStringBufSize]byte

// concatstrings implements a Go string concatenation x+y+z+...
// The operands are passed in the slice a.
// If buf != nil, the compiler has determined that the result does not
// escape the calling function, so the string data can be stored in buf
// if small enough.
func concatstrings(buf *tmpBuf, a []string) string {
	idx := 0
	l := 0     // 记录总共concat了多长的字符
	count := 0 // 记录concat了几个有效的string
	for i, x := range a {
		n := len(x)
		if n == 0 { // 长度为0，无效字符串
			continue
		}
		if l+n < l {
			throw("string concatenation too long")
		}
		l += n
		count++
		idx = i
	}
	if count == 0 { // 如果都是长度为0的无效字符串，则直接返回空字符串
		return ""
	}

	// If there is just one string and either it is not on the stack
	// or our result does not escape the calling frame (buf != nil),
	// then we can return that string directly.
	if count == 1 && (buf != nil || !stringDataOnStack(a[idx])) {
		return a[idx]
	}
	s, b := rawstringtmp(buf, l) // 新初始化一个string以及对应的slice，自动选择分配栈上还是堆上
	for _, x := range a {        // 数据复制到底层数组，注意：这里是虽然是复制到slice的底层数组，但是string跟slice指向了同一个底层数组
		copy(b, x)
		b = b[len(x):]
	}
	return s
}

func concatstring2(buf *tmpBuf, a [2]string) string { // a+=b会翻译成这个方法，第二个参数是a和b两个string
	return concatstrings(buf, a[:])
}

func concatstring3(buf *tmpBuf, a [3]string) string {
	return concatstrings(buf, a[:])
}

func concatstring4(buf *tmpBuf, a [4]string) string {
	return concatstrings(buf, a[:])
}

func concatstring5(buf *tmpBuf, a [5]string) string {
	return concatstrings(buf, a[:])
}

// Buf is a fixed-size buffer for the result,
// it is not nil if the result does not escape.
func slicebytetostring(buf *tmpBuf, b []byte) (str string) { // string([]byte{}) 会翻译成这个函数
	l := len(b)
	if l == 0 {
		// Turns out to be a relatively common case.
		// Consider that you want to parse out data between parens in "foo()bar",
		// you find the indices and convert the subslice to string.
		return ""
	}
	if raceenabled {
		racereadrangepc(unsafe.Pointer(&b[0]),
			uintptr(l),
			getcallerpc(),
			funcPC(slicebytetostring))
	}
	if msanenabled {
		msanread(unsafe.Pointer(&b[0]), uintptr(l))
	}
	if l == 1 {
		stringStructOf(&str).str = unsafe.Pointer(&staticbytes[b[0]])
		stringStructOf(&str).len = 1
		return
	}

	var p unsafe.Pointer
	if buf != nil && len(b) <= len(buf) {
		p = unsafe.Pointer(buf) // 32字节以内栈中分配
	} else {
		p = mallocgc(uintptr(len(b)), nil, false) // 堆中分配
	}
	stringStructOf(&str).str = p
	stringStructOf(&str).len = len(b)
	memmove(p, (*(*slice)(unsafe.Pointer(&b))).array, uintptr(len(b))) // b slice的底层数组拷贝到p指向的位置
	return
}

// stringDataOnStack reports whether the string's data is
// stored on the current goroutine's stack.
func stringDataOnStack(s string) bool { // 检查string是否分配在栈中
	ptr := uintptr(stringStructOf(&s).str) // 拿到指向数据的指针值
	stk := getg().stack
	return stk.lo <= ptr && ptr < stk.hi // 判断指针值是否在g的栈内
}

func rawstringtmp(buf *tmpBuf, l int) (s string, b []byte) {
	if buf != nil && l <= len(buf) { // 如果string长度不大于32个字节，则分配在栈上，否则分配到堆中
		b = buf[:l]
		s = slicebytetostringtmp(b)
	} else {
		s, b = rawstring(l)
	}
	return
}

// slicebytetostringtmp returns a "string" referring to the actual []byte bytes.
//
// Callers need to ensure that the returned string will not be used after
// the calling goroutine modifies the original slice or synchronizes with
// another goroutine.
//
// The function is only called when instrumenting
// and otherwise intrinsified by the compiler.
//
// Some internal compiler optimizations use this function.
// - Used for m[T1{... Tn{..., string(k), ...} ...}] and m[string(k)]
//   where k is []byte, T1 to Tn is a nesting of struct and array literals.
// - Used for "<"+string(b)+">" concatenation where b is []byte.
// - Used for string(b)=="foo" comparison where b is []byte.
func slicebytetostringtmp(b []byte) string {
	if raceenabled && len(b) > 0 {
		racereadrangepc(unsafe.Pointer(&b[0]),
			uintptr(len(b)),
			getcallerpc(),
			funcPC(slicebytetostringtmp))
	}
	if msanenabled && len(b) > 0 {
		msanread(unsafe.Pointer(&b[0]), uintptr(len(b)))
	}
	return *(*string)(unsafe.Pointer(&b))
}

func stringtoslicebyte(buf *tmpBuf, s string) []byte { // string强转成[]byte会翻译成这个方法。如果是[]byte("123")这种，则编译器会直接转化"123"，不会翻译成这个方法了
	var b []byte
	if buf != nil && len(s) <= len(buf) {
		*buf = tmpBuf{}  // 32字节以内栈中分配
		b = buf[:len(s)]
	} else {
		b = rawbyteslice(len(s))
	}
	copy(b, s)  // 底层数组拷贝。因为string的底层数组就是byte数组，所以这里直接拷贝就行了
	return b
}

func stringtoslicerune(buf *[tmpStringBufSize]rune, s string) []rune { // string强转成[]rune会翻译成这个方法
	// two passes.
	// unlike slicerunetostring, no race because strings are immutable.
	n := 0
	for range s {  // 计算出string中有几个rune字符
		n++
	}

	var a []rune
	if buf != nil && n <= len(buf) {  // 32个rune字符以内，分配栈中
		*buf = [tmpStringBufSize]rune{}
		a = buf[:n]
	} else {
		a = rawruneslice(n)
	}

	n = 0
	for _, r := range s {  // range出来的都是rune
		a[n] = r
		n++
	}
	return a
}

func slicerunetostring(buf *tmpBuf, a []rune) string { // []rune强转成string会翻译成这个方法
	if raceenabled && len(a) > 0 {
		racereadrangepc(unsafe.Pointer(&a[0]),
			uintptr(len(a))*unsafe.Sizeof(a[0]),
			getcallerpc(),
			funcPC(slicerunetostring))
	}
	if msanenabled && len(a) > 0 {
		msanread(unsafe.Pointer(&a[0]), uintptr(len(a))*unsafe.Sizeof(a[0]))
	}
	var dum [4]byte
	size1 := 0
	for _, r := range a {  // 计算出rune数组中所有rune转换成bytes总共占用多少字节
		size1 += encoderune(dum[:], r)
	}
	s, b := rawstringtmp(buf, size1+3)  // 分配新空间
	size2 := 0
	for _, r := range a {
		// check for race
		if size2 >= size1 {
			break
		}
		size2 += encoderune(b[size2:], r)  // rune转换成bytes写入b中，也就是写入了s中
	}
	return s[:size2]
}

type stringStruct struct {
	str unsafe.Pointer // 指向底层数组的指针
	len int            // 底层数组大小
}

// Variant with *byte pointer type for DWARF debugging.
type stringStructDWARF struct {
	str *byte
	len int
}

func stringStructOf(sp *string) *stringStruct {
	return (*stringStruct)(unsafe.Pointer(sp))
}

func intstring(buf *[4]byte, v int64) (s string) { // int强转成string会翻译成这个方法
	if v >= 0 && v < runeSelf {
		stringStructOf(&s).str = unsafe.Pointer(&staticbytes[v])
		stringStructOf(&s).len = 1
		return
	}

	var b []byte
	if buf != nil {
		b = buf[:]
		s = slicebytetostringtmp(b)
	} else {
		s, b = rawstring(4)  // 分配4个字节大小的string，因为一个rune字符最多占用4个字节
	}
	if int64(rune(v)) != v {
		v = runeError
	}
	n := encoderune(b, rune(v))  // 整型先转换成rune，然后转换成string
	return s[:n]
}

// rawstring allocates storage for a new string. The returned
// string and byte slice both refer to the same storage.
// The storage is not zeroed. Callers should use
// b to set the string contents and then drop b.
func rawstring(size int) (s string, b []byte) { // 堆上初始化一个string以及一个对应的slice，他们指向同一个底层数组
	p := mallocgc(uintptr(size), nil, false) // 堆上分配底层数组

	stringStructOf(&s).str = p    // 设置指向底层数组的指针
	stringStructOf(&s).len = size // 设置数组大小

	*(*slice)(unsafe.Pointer(&b)) = slice{p, size, size}

	return
}

// rawbyteslice allocates a new byte slice. The byte slice is not zeroed.
func rawbyteslice(size int) (b []byte) {
	cap := roundupsize(uintptr(size))
	p := mallocgc(cap, nil, false)
	if cap != uintptr(size) {
		memclrNoHeapPointers(add(p, uintptr(size)), cap-uintptr(size))
	}

	*(*slice)(unsafe.Pointer(&b)) = slice{p, size, int(cap)}
	return
}

// rawruneslice allocates a new rune slice. The rune slice is not zeroed.
func rawruneslice(size int) (b []rune) {
	if uintptr(size) > maxAlloc/4 {
		throw("out of memory")
	}
	mem := roundupsize(uintptr(size) * 4)
	p := mallocgc(mem, nil, false)
	if mem != uintptr(size)*4 {
		memclrNoHeapPointers(add(p, uintptr(size)*4), mem-uintptr(size)*4)
	}

	*(*slice)(unsafe.Pointer(&b)) = slice{p, size, int(mem / 4)}
	return
}

// used by cmd/cgo
func gobytes(p *byte, n int) (b []byte) {
	if n == 0 {
		return make([]byte, 0)
	}

	if n < 0 || uintptr(n) > maxAlloc {
		panic(errorString("gobytes: length out of range"))
	}

	bp := mallocgc(uintptr(n), nil, false)
	memmove(bp, unsafe.Pointer(p), uintptr(n))

	*(*slice)(unsafe.Pointer(&b)) = slice{bp, n, n}
	return
}

// This is exported via linkname to assembly in syscall (for Plan9).
//go:linkname gostring
func gostring(p *byte) string {
	l := findnull(p)
	if l == 0 {
		return ""
	}
	s, b := rawstring(l)
	memmove(unsafe.Pointer(&b[0]), unsafe.Pointer(p), uintptr(l))
	return s
}

func gostringn(p *byte, l int) string {
	if l == 0 {
		return ""
	}
	s, b := rawstring(l)
	memmove(unsafe.Pointer(&b[0]), unsafe.Pointer(p), uintptr(l))
	return s
}

func index(s, t string) int {
	if len(t) == 0 {
		return 0
	}
	for i := 0; i < len(s); i++ {
		if s[i] == t[0] && hasPrefix(s[i:], t) {
			return i
		}
	}
	return -1
}

func contains(s, t string) bool {
	return index(s, t) >= 0
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

const (
	maxUint = ^uint(0)
	maxInt  = int(maxUint >> 1)
)

// atoi parses an int from a string s.
// The bool result reports whether s is a number
// representable by a value of type int.
func atoi(s string) (int, bool) {  // string转换成整型（字面上的转换，"12"变成12）
	if s == "" {
		return 0, false
	}

	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}

	un := uint(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if un > maxUint/10 {
			// overflow
			return 0, false
		}
		un *= 10
		un1 := un + uint(c) - '0'
		if un1 < un {
			// overflow
			return 0, false
		}
		un = un1
	}

	if !neg && un > uint(maxInt) {
		return 0, false
	}
	if neg && un > uint(maxInt)+1 {
		return 0, false
	}

	n := int(un)
	if neg {
		n = -n
	}

	return n, true
}

// atoi32 is like atoi but for integers
// that fit into an int32.
func atoi32(s string) (int32, bool) {
	if n, ok := atoi(s); n == int(int32(n)) {
		return int32(n), ok
	}
	return 0, false
}

//go:nosplit
func findnull(s *byte) int {
	if s == nil {
		return 0
	}

	// Avoid IndexByteString on Plan 9 because it uses SSE instructions
	// on x86 machines, and those are classified as floating point instructions,
	// which are illegal in a note handler.
	if GOOS == "plan9" {
		p := (*[maxAlloc/2 - 1]byte)(unsafe.Pointer(s))
		l := 0
		for p[l] != 0 {
			l++
		}
		return l
	}

	// pageSize is the unit we scan at a time looking for NULL.
	// It must be the minimum page size for any architecture Go
	// runs on. It's okay (just a minor performance loss) if the
	// actual system page size is larger than this value.
	const pageSize = 4096

	offset := 0
	ptr := unsafe.Pointer(s)
	// IndexByteString uses wide reads, so we need to be careful
	// with page boundaries. Call IndexByteString on
	// [ptr, endOfPage) interval.
	safeLen := int(pageSize - uintptr(ptr)%pageSize)

	for {
		t := *(*string)(unsafe.Pointer(&stringStruct{ptr, safeLen}))
		// Check one page at a time.
		if i := bytealg.IndexByteString(t, 0); i != -1 {
			return offset + i
		}
		// Move to next page
		ptr = unsafe.Pointer(uintptr(ptr) + uintptr(safeLen))
		offset += safeLen
		safeLen = pageSize
	}
}

func findnullw(s *uint16) int {
	if s == nil {
		return 0
	}
	p := (*[maxAlloc/2/2 - 1]uint16)(unsafe.Pointer(s))
	l := 0
	for p[l] != 0 {
		l++
	}
	return l
}

//go:nosplit
func gostringnocopy(str *byte) string {
	ss := stringStruct{str: unsafe.Pointer(str), len: findnull(str)}
	s := *(*string)(unsafe.Pointer(&ss))
	return s
}

func gostringw(strw *uint16) string {
	var buf [8]byte
	str := (*[maxAlloc/2/2 - 1]uint16)(unsafe.Pointer(strw))
	n1 := 0
	for i := 0; str[i] != 0; i++ {
		n1 += encoderune(buf[:], rune(str[i]))
	}
	s, b := rawstring(n1 + 4)
	n2 := 0
	for i := 0; str[i] != 0; i++ {
		// check for race
		if n2 >= n1 {
			break
		}
		n2 += encoderune(b[n2:], rune(str[i]))
	}
	b[n2] = 0 // for luck
	return s[:n2]
}

// parseRelease parses a dot-separated version number. It follows the
// semver syntax, but allows the minor and patch versions to be
// elided.
func parseRelease(rel string) (major, minor, patch int, ok bool) {
	// Strip anything after a dash or plus.
	for i := 0; i < len(rel); i++ {
		if rel[i] == '-' || rel[i] == '+' {
			rel = rel[:i]
			break
		}
	}

	next := func() (int, bool) {
		for i := 0; i < len(rel); i++ {
			if rel[i] == '.' {
				ver, ok := atoi(rel[:i])
				rel = rel[i+1:]
				return ver, ok
			}
		}
		ver, ok := atoi(rel)
		rel = ""
		return ver, ok
	}
	if major, ok = next(); !ok || rel == "" {
		return
	}
	if minor, ok = next(); !ok || rel == "" {
		return
	}
	patch, ok = next()
	return
}
