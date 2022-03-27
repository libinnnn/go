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
	state int32  // 锁状态，0bit表示锁已被获取，1bit表示有Groutine被唤醒，尝试获取锁，第二个bit标记当前锁是否处于饥饿状态；由于该字段会被多个Goroutine使用，所以通过cas来保证原子性
	sema  uint32 // 唤醒Goroutine所用到的信号量
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
	// 公平锁
	//
	// 锁有两种模式：正常模式和饥饿模式。
	// 在正常模式下，所有的等待锁的goroutine都会存在一个先进先出的队列中（轮流被唤醒）
	// 但是一个被唤醒的goroutine并不是直接获得锁，而是仍然需要和那些新请求锁的（new arrivial）
	// 的goroutine竞争，而这其实是不公平的，因为新请求锁的goroutine有一个优势——它们正在CPU上
	// 运行，并且数量可能会很多。所以一个被唤醒的goroutine拿到锁的概率是很小的。在这种情况下，
	// 这个被唤醒的goroutine会加入到队列的头部。如果一个等待的goroutine有超过1ms（写死在代码中）
	// 都没获取到锁，那么就会把锁转变为饥饿模式。
	//
	// 在饥饿模式中，锁的所有权会直接从释放锁(unlock)的goroutine转交给队列头的goroutine，
	// 新请求锁的goroutine就算锁是空闲状态也不会去获取锁，并且也不会尝试自旋。它们只是排到队列的尾部。
	//
	// 如果一个goroutine获取到了锁之后，它会判断以下两种情况：
	// 1. 它是队列中最后一个goroutine；
	// 2. 它拿到锁所花的时间小于1ms；
	// 以上只要有一个成立，它就会把锁转变回正常模式。

	// 正常模式会有比较好的性能，因为即使有很多阻塞的等待锁的goroutine，
	// 一个goroutine也可以尝试请求多次锁。
	// 饥饿模式对于防止尾部延迟来说非常的重要。
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	// 是否饥饿
	starving := false
	// 是否唤醒
	awoke := false
	// 自旋次数
	iter := 0
	// 锁的状态
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 饥饿状态不自旋，因为所有权会直接交接给等待的Goroutine，我们获取不到对应的锁
		// 当有g持有锁并且当前不是饥饿状态进入循环
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			// 将自己的状态以及锁的状态置为唤醒，这样当unlock就不会唤醒其他被阻塞的Goroutine了
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}
		// 根据当前的状态来设置期望的状态
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 如果不是饥饿状态的话，就把期望状态的锁状态设置为占用锁
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		// 如果锁是获取或者饥饿状态的话，就把等待队列长度+1（这里实际是+8，因为是第四个比特位开始）
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 如果说当前的goroutine是饥饿状态，并且锁被其它goroutine获取
		// 那么将期望的锁的状态设置为饥饿状态
		// 如果锁是释放状态，那么就不用切换了
		// Unlock期望一个饥饿的锁会有一些等待拿锁的goroutine，而不只是一个
		// 这种情况下不会成立
		// 如果是饥饿并且持有锁，设置饥饿状态位
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		// 如果当前g被唤醒，需要重置这个状态
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 设置new为非唤醒状态
			new &^= mutexWoken
		}
		// cas设置锁的状态
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 如果old状态不是获取锁的状态不是饥饿状态，代表当前g已经成功获取锁
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 如果之前等待过，需要放到队列头
			queueLifo := waitStartTime != 0
			// 如果未等待过，设置等待时间
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			// 既然获取锁失败了，就使用sleep原语来阻塞当前goroutine
			// 通过信号量来排队获取锁
			// 如果是新来的goroutine，就放到队列尾部
			// 如果是被唤醒的等待锁的goroutine，就放到队列头部
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			// 如果是饥饿状态或者等待了一毫秒以上，设置为饥饿状态
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			// 再次获取锁的状态
			old = m.state
			// 如果是饥饿状态
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 当前的g获取了锁, 等到队列长度减1
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 如果当前非饥饿状态或者g是队列中最后1个g，则退出饥饿模式，设置状态为正常
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			// 如果不是饥饿模式，设置为已唤醒，重置循环次数
			awoke = true
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
	// 新状态的值为当前的状态减去持有状态
	new := atomic.AddInt32(&m.state, -mutexLocked)
	// 如果为0，则代表无等待，无饥饿，无占有，则此时可以正常返回；否则进入慢解锁过程
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 释放未上锁的锁，则会引发异常
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	// 如果是不饥饿的情况
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果等待队列长度为0或者old状态是持有的或者唤醒的或者饥饿的，则直接返回
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 锁的状态置为被唤醒并且等待队列-1，通过信号量唤醒一个阻塞的g去获取锁
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			// cas设置失败则重新获取state值
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 如果是饥饿状态下，那么我们就直接把锁的所有权通过信号量移交给队列头的goroutine就好了
		// handoff = true表示直接把锁交给队列头部的goroutine
		// 注意：在这个时候，锁被获取的状态没有被设置，会由被唤醒的goroutine在唤醒后设置
		// 但是当锁处于饥饿状态的时候，我们也认为锁是被获取的(因为我们手动指定了获取的goroutine)
		// 所以说新来的goroutine不会尝试去获取锁(在Lock中有体现)
		runtime_Semrelease(&m.sema, true, 1)
	}
}
