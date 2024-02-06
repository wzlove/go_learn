// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"std/internal/race"
	"sync/atomic"
	"unsafe"
)

// A WaitGroup waits for a collection of goroutines to finish.
// The main goroutine calls Add to set the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls Done when finished. At the same time,
// Wait can be used to block until all goroutines have finished.
//
// A WaitGroup must not be copied after first use.
//
// In the terminology of the Go memory model, a call to Done
// “synchronizes before” the return of any Wait call that it unblocks.
type WaitGroup struct {
	//不允许进行拷贝
	noCopy noCopy

	state atomic.Uint64 // high 32 bits are counter, low 32 bits are waiter count.
	sema  uint32
}

// Add adds delta, which may be negative, to the WaitGroup counter.
// If the counter becomes zero, all goroutines blocked on Wait are released.
// If the counter goes negative, Add panics.
//
// Note that calls with a positive delta that occur when the counter is zero
// must happen before a Wait. Calls with a negative delta, or calls with a
// positive delta that start when the counter is greater than zero, may happen
// at any time.
// Typically, this means the calls to Add should execute before the statement
// creating the goroutine or other event to be waited for.
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned.
// See the WaitGroup example.
func (wg *WaitGroup) Add(delta int) {
	if race.Enabled {
		if delta < 0 {
			// Synchronize decrements with Wait.
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}
	//wg.state 的计数器加上 delta (加到 state的高32位上)
	state := wg.state.Add(uint64(delta) << 32) //高 32 位加上delta
	v := int32(state >> 32)                    //高 32 位 (counter)
	w := uint32(state)                         //低 32 位（waiter）
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		race.Read(unsafe.Pointer(&wg.sema))
	}
	//计数器不能为负数（加上 delta 之后不能为负数，最小只能到 0）
	if v < 0 {
		panic("sync: negative WaitGroup counter")
	}
	// 正常使用情况下，是先调用 Add 再调用 Wait 的，这种情况下，w 是 0，v > 0
	if w != 0 && delta > 0 && v == int32(delta) {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	//  v > 0，计数器大于 0
	//  w == 0，没有在 Wait 的协程
	//  说明还没有到唤醒 waiter 的时候
	if v > 0 || w == 0 {
		return
	}

	// Add 负数的时候，v 会减去对应的数值，减到最后 v 是 0。
	// 计数器是 0，并且有等待的协程，现在要唤醒这些协程。

	// 存在等待的协程时，goroutine 已将计数器设置为0。
	// 现在不可能同时出现状态突变：
	// - Add 不能与 Wait 同时发生，
	// - 如果看到计数器==0，则 Wait 不会增加等待的协程。
	// 仍然要做一个廉价的健康检查，以检测 WaitGroup 的误用。
	// This goroutine has set counter to 0 when waiters > 0.
	// Now there can't be concurrent mutations of state:
	// - Adds must not happen concurrently with Wait,
	// - Wait does not increment waiters if it sees counter == 0.
	// Still do a cheap sanity check to detect WaitGroup misuse.
	if wg.state.Load() != state {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// Reset waiters count to 0.
	wg.state.Store(0)
	for ; w != 0; w-- {
		// signal，调用 Wait 的地方会解除阻塞
		runtime_Semrelease(&wg.sema, false, 0)
	}
}

// Done decrements the WaitGroup counter by one.
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait blocks until the WaitGroup counter is zero.
func (wg *WaitGroup) Wait() {
	if race.Enabled {
		race.Disable()
	}
	for {
		// 获取当前计数器
		state := wg.state.Load()
		// 计数器
		v := int32(state >> 32)
		// waiter 数量
		w := uint32(state)
		// v 为 0，不需要等待，直接返回
		if v == 0 {
			// Counter is 0, no need to wait.
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
		// 增加 waiter 数量。
		// 调用一次 Wait，waiter 数量会加 1。
		// Increment waiters count.
		if wg.state.CompareAndSwap(state, state+1) {
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				race.Write(unsafe.Pointer(&wg.sema))
			}
			// 这会阻塞，直到 sema (信号量)大于 0
			runtime_Semacquire(&wg.sema) // goparkunlock
			// state 不等 0
			// wait 还没有返回又继续使用了 WaitGroup
			if wg.state.Load() != 0 {
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			// 解除阻塞状态了，可以返回了
			return
		}
		// 状态没有修改成功（state 没有成功 +1），开始下一次尝试。
	}
}
