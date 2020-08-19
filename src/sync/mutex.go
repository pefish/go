// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	state int32  // 前29位表示阻塞等待锁的个数，第30位表示该Mutex是否处理饥饿模式，第31位表示是否有m已被唤醒，最后一位表示该Mutex是否已被锁定
	sema  uint32  // 指向sema根
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // mutex is locked
	mutexWoken
	mutexStarving
	mutexWaiterShift = iota

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {  // 原子操作：比较&m.state是否等于0，等于的话&m.state设置为mutexLocked，不等于的话返回false。所以同一个mutex必然只有一个走进if里面
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return  // 直接返回
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()  // 其他没抢到锁的g会执行这里
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {  // 如果锁已被抢走且非mutexStarving状态且g可以自旋
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {  // 设置为已有g被唤醒
				awoke = true
			}
			runtime_doSpin()  // cpu自旋一会儿（就是睡眠）
			iter++
			old = m.state
			continue  // 上面这个自旋过程不会超过4次（runtime_canSpin中有限制），等一会儿为的是在这个期间锁被释放后，本g能够立马拿到锁，不用进入无谓的阻塞等待
		}
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 {  // 如果非饥饿模式，则mutex变为已上锁
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {  // 如果锁住了或者处于饥饿模式
			new += 1 << mutexWaiterShift  // waiter+1
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		if starving && old&mutexLocked != 0 {  // 如果有阻塞中的g饥饿了且mutex锁住了，则mutex切换饥饿模式
			new |= mutexStarving
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken   // 按位清空。mutexWoken位为0的话，置为0，否则不变。这里其实多余，mutexWoken位肯定是1，是0的话上面就throw了
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {  // 变更mutex到新属性
			if old&(mutexLocked|mutexStarving) == 0 {  // 变更前没有锁且没处于饥饿模式，就退出（大部分情况下，自旋期间锁就已被释放，这里就会退出，本g就直接抢到锁了，不用进入等待）
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.  执行到这里就表示自旋过程没有抢到锁
			queueLifo := waitStartTime != 0 // 表示本g之前有没有阻塞过
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()  // 记录开始等待的时间
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)  // g休眠，当被唤醒时继续执行。这里queueLifo为true的话，g会立马被调度
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs  // 是否饥饿
			old = m.state
			if old&mutexStarving != 0 {  // 如果处于饥饿模式
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)  // waiter-1
				if !starving || old>>mutexWaiterShift == 1 {  // 如果自己是最后一个waiter，那么清除 mutexStarving 标记
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving  // 退出饥饿模式
				}
				atomic.AddInt32(&m.state, delta)
				break  // 抢锁成功，退出
			}
			awoke = true  // 如果不是饥饿模式，被唤醒后还得继续去抢锁，有可能还是抢不到
			iter = 0
		} else {
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)  // locked标记位置为0。这里是通过减1实现的，配合195行可以防止同一个锁被多次unlock
	if new != 0 {  // 如果结果是0，说明其他标记都是0，不需要做其他操作了
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 {  // 配合186行可以防止同一个锁被多次unlock
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {  // 如果Mutex没有处于饥饿模式（有g阻塞时间超过1ms了，mutex就会切换到饥饿模式）
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {  // 饥饿模式下
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		runtime_Semrelease(&m.sema, true, 1)  // 释放锁，且让拿到锁的处于饥饿状态的g立马在当前m下继续执行
	}
}
