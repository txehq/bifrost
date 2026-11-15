/*
 * Copyright 2023 ByteDance Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
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
	"testing"
	"time"
)

func Test_registerModuleRace(t *testing.T) {
	n, parallel := 100, 8
	s := &moduledata{}
	registerModule(s)
	wg := sync.WaitGroup{}
	wg.Add(parallel)
	filler := func(n int) {
		defer wg.Done()
		for i := 0; i < n; i++ {
			registerModule(&moduledata{})
		}
	}
	for i := 0; i < parallel; i++ {
		go filler(n)
	}
	wg.Wait()
	i := 0
	for p := s.next; p != nil; p = p.next {
		i += 1
	}
	if i != parallel*n {
		t.Errorf("got %v, expected %v", i, parallel*n)
	}
}

// reachable reports whether mod is on the list that starts at head.
func reachable(head, mod *moduledata) bool {
	for p := head; p != nil; p = loadModule(&p.next) {
		if p == mod {
			return true
		}
	}
	return false
}

// TXE-7504: a registration must never return while its module is unreachable
// from the head of the list. Upstream's lock-free version let a second
// registration finish while the first sat between its tail CAS and its link,
// which left the second module (and its JIT code) invisible to
// runtime.findfunc. Here the first registration is held at the gap; the second
// must wait for it, and both modules must be reachable when they return.
func Test_registerModuleNeverPublishesAnUnreachableModule(t *testing.T) {
	head := &moduledata{}
	tail := head
	first, second := &moduledata{}, &moduledata{}

	inGap := make(chan struct{})
	release := make(chan struct{})
	registerGapHook = func() {
		registerGapHook = nil // only the first registration stops here
		close(inGap)
		<-release
	}
	defer func() { registerGapHook = nil }()

	firstDone := make(chan struct{})
	go func() {
		registerModuleSerialized(&tail, first)
		close(firstDone)
	}()
	<-inGap

	secondDone := make(chan struct{})
	go func() {
		registerModuleSerialized(&tail, second)
		close(secondDone)
	}()

	select {
	case <-secondDone:
		if !reachable(head, second) {
			t.Fatal("second registration returned with its module unreachable from the list head")
		}
		t.Fatal("second registration returned while the first was still between link and publish")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-firstDone
	<-secondDone
	if !reachable(head, first) || !reachable(head, second) {
		t.Fatalf("after both registrations: first reachable=%v, second reachable=%v",
			reachable(head, first), reachable(head, second))
	}
	if loadModule(&head.next) != first || loadModule(&first.next) != second || tail != second {
		t.Fatal("registrations were not appended in order")
	}
}

// TXE-7504: concurrent registrations on a private list all end up reachable,
// each exactly once, with the tail at the last one.
func Test_registerModuleSerializedConcurrent(t *testing.T) {
	head := &moduledata{}
	tail := head
	n, parallel := 200, 16
	wg := sync.WaitGroup{}
	wg.Add(parallel)
	for i := 0; i < parallel; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				m := &moduledata{}
				registerModuleSerialized(&tail, m)
				if !reachable(head, m) {
					t.Error("registration returned with its module unreachable")
					return
				}
			}
		}()
	}
	wg.Wait()
	count := 0
	var last *moduledata
	for p := head.next; p != nil; p = p.next {
		count++
		last = p
	}
	if count != parallel*n || last != tail {
		t.Fatalf("got %d modules (tail is last: %v), want %d", count, last == tail, parallel*n)
	}
}
