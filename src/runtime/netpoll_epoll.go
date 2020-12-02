// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package runtime

import "unsafe"

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32

//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
func closeonexec(fd int32)

var (
	epfd int32 = -1 // epoll descriptor  epoll实例的文件描述符

	netpollBreakRd, netpollBreakWr uintptr // for netpollBreak
)

func netpollinit() {  // netpoll的初始化
	epfd = epollcreate1(_EPOLL_CLOEXEC)  // 系统调用 epoll_create1 创建一个epoll实例并返回关联的文件描述符，同时指定_EPOLL_CLOEXEC使此文件描述符在被子进程复制后exec执行前自动被关闭
	if epfd < 0 {  // 如果epollcreate1系统调用不可用，则使用更老的epollcreate系统调用
		epfd = epollcreate(1024)
		if epfd < 0 {
			println("runtime: epollcreate failed with", -epfd)
			throw("runtime: netpollinit failed")
		}
		closeonexec(epfd)  // 通过 fcntl 系统调用设置_EPOLL_CLOEXEC标示，跟上述一样效果
	}
	r, w, errno := nonblockingPipe()  // 通过pipe系统调用创建一个非阻塞通道
	if errno != 0 {
		println("runtime: pipe failed with", -errno)
		throw("runtime: pipe failed")
	}
	ev := epollevent{
		events: _EPOLLIN,
	}
	*(**uintptr)(unsafe.Pointer(&ev.data)) = &netpollBreakRd  // 填充ev.data。这个事件就是监听netpollBreakRd是否可读（_EPOLLIN），可读的话，内核就将ev.data返回
	errno = epollctl(epfd, _EPOLL_CTL_ADD, r, &ev)  // 使用epollctl系统调用，_EPOLL_CTL_ADD标志进行添加事件监听的动作。这个事件的监听是为了打破epollwait取事件的死循环
	if errno != 0 {
		println("runtime: epollctl failed with", -errno)
		throw("runtime: epollctl failed")
	}
	netpollBreakRd = uintptr(r)
	netpollBreakWr = uintptr(w)
}

func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == uintptr(epfd) || fd == netpollBreakRd || fd == netpollBreakWr
}

func netpollopen(fd uintptr, pd *pollDesc) int32 {  // 添加一个事件监听（监听类型有可读、可写、链接关闭），采用_EPOLLET模式让操作系统只通知一次
	var ev epollevent
	ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
}

func netpollclose(fd uintptr) int32 {
	var ev epollevent
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// netpollBreak interrupts an epollwait.
func netpollBreak() {
	for {
		var b byte
		n := write(netpollBreakWr, unsafe.Pointer(&b), 1)  // 通过向netpollBreakWr通道写数据，触发epoll初始化时监听的事件，从而打破epollwait退出向操作系统取事件的死循环
		if n == 1 {
			break
		}
		if n == -_EINTR {
			continue
		}
		if n == -_EAGAIN {
			return
		}
		println("runtime: netpollBreak write failed with", -n)
		throw("runtime: netpollBreak write failed")
	}
}

// netpoll checks for ready network connections.
// Returns list of goroutines that become runnable.
// delay < 0: blocks indefinitely
// delay == 0: does not block, just polls
// delay > 0: block for up to that many nanoseconds
func netpoll(delay int64) gList {  // 调度器或者ssymon都会在一定条件下向操作系统取一波事件，并不是单独线程一直取
	if epfd == -1 {
		return gList{}
	}
	var waitms int32
	if delay < 0 {
		waitms = -1
	} else if delay == 0 {
		waitms = 0
	} else if delay < 1e6 {
		waitms = 1
	} else if delay < 1e15 {
		waitms = int32(delay / 1e6)
	} else {
		// An arbitrary cap on how long to wait for a timer.
		// 1e9 ms == ~11.5 days.
		waitms = 1e9
	}
	var events [128]epollevent
retry:
	n := epollwait(epfd, &events[0], int32(len(events)), waitms)  // waitms为-1，则此处阻塞，为0则非阻塞，非-1也非0则阻塞waitms这么多ms。运行时中waitms基本都是传的0。这里无需考虑惊群问题，因为Go中线程本就不多而且这里是异步的。这里如果多个线程阻塞式执行到这里，则会有惊群问题，操作系统保证只有一个线程收到真正的事件，其他线程收到EAGAIN错误
	if n < 0 {
		if n != -_EINTR {
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		// If a timed sleep was interrupted, just return to
		// recalculate how long we should sleep now.
		if waitms > 0 {
			return gList{}
		}
		goto retry
	}
	var toRun gList  // 一个单链表
	for i := int32(0); i < n; i++ {
		ev := &events[i]
		if ev.events == 0 {
			continue
		}

		if *(**uintptr)(unsafe.Pointer(&ev.data)) == &netpollBreakRd {  // 如果被触发的事件是epoll初始化时监听的那个事件
			if ev.events != _EPOLLIN {  // 事件类型一定是读通道可读，因为epoll初始化时监听的就只是读通道可读
				println("runtime: netpoll: break fd ready for", ev.events)
				throw("runtime: netpoll: break fd ready for something unexpected")
			}
			if delay != 0 {
				// netpollBreak could be picked up by a
				// nonblocking poll. Only read the byte
				// if blocking.
				var tmp [16]byte
				read(int32(netpollBreakRd), noescape(unsafe.Pointer(&tmp[0])), int32(len(tmp)))  // 将pipe中数据读出来
			}
			continue
		}

		var mode int32
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'r'
		}
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'w'
		}
		if mode != 0 {
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data))
			pd.everr = false
			if ev.events == _EPOLLERR {
				pd.everr = true
			}
			netpollready(&toRun, pd, mode)
		}
	}
	return toRun
}
