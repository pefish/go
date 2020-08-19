// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build freebsd linux netbsd openbsd solaris

package runtime

func nonblockingPipe() (r, w int32, errno int32) {
	r, w, errno = pipe2(_O_NONBLOCK | _O_CLOEXEC)
	if errno == -_ENOSYS {
		r, w, errno = pipe()
		if errno != 0 {
			return -1, -1, errno
		}
		closeonexec(r)  // 设置_O_CLOEXEC标志
		setNonblock(r)  // 设置_O_NONBLOCK标志
		closeonexec(w)
		setNonblock(w)
	}
	return r, w, errno
}
