//go:build !bytedance_tango
// +build !bytedance_tango

/**
 * Copyright 2024 ByteDance Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package loader

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// TXE-7504 (txehq/bifrost patch to loader v0.5.2): registration is serialized,
// and a module is linked into the runtime's module list before it becomes the
// tail.
//
// Upstream's lock-free version CASed lastmoduledatap to the new module first
// and linked oldTail.next second. A goroutine preempted between those two
// steps left its module unreachable from runtime.firstmoduledata; a second
// registration in that gap linked onto the orphan, returned, and ran JIT code
// whose PCs runtime.findfunc could not resolve. The next stack growth through
// such a frame aborted the process with "fatal error: unknown caller pc".
// The encoder and decoder JIT caches lock separately, so their registrations
// interleave. Upstream's bytedance_tango variant (register_tango.go) already
// links under a lock; this applies the same ordering to the default build.
var registerModuleMu sync.Mutex

// registerGapHook runs between linking a module and publishing it as the tail.
// Tests set it to hold a registration at that point; it is nil otherwise.
var registerGapHook func()

func registerModule(mod *moduledata) {
	registerModuleSerialized(&lastmoduledatap, mod)
}

func registerModuleSerialized(tail **moduledata, mod *moduledata) {
	registerModuleMu.Lock()
	defer registerModuleMu.Unlock()
	oldTail := loadModule(tail)
	storeModule(&oldTail.next, mod)
	if registerGapHook != nil {
		registerGapHook()
	}
	storeModule(tail, mod)
}

func loadModule(p **moduledata) *moduledata {
	return (*moduledata)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(p))))
}

func storeModule(p **moduledata, value *moduledata) {
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(p)), unsafe.Pointer(value))
}
