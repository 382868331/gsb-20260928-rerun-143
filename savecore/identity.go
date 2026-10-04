package savecore

import (
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
)

// Identity tokens replace raw pointer addresses as object identity.
// Go may reuse the address of a collected allocation for a brand-new
// object; keying stable ids on uintptr would then let a new object
// inherit the stable id of an unrelated, dead one.

type identityTokens struct {
	mu    sync.Mutex
	m     map[uintptr]uint64 // live object address -> generation token
	known map[uint64]string  // generation token -> stable id
}

func newIdentityTokens() *identityTokens {
	return &identityTokens{m: map[uintptr]uint64{}, known: map[uint64]string{}}
}

var globalTokenSeq uint64

// addrToken returns the generation token for a live object, minting
// and installing a fresh one for a previously unseen address. The
// global addr -> token entry is removed by the object's finalizer so
// an address reused after collection gets a new generation, while
// tokens themselves are never reused.
var addrTokens sync.Map // uintptr -> uint64

func tokenOf(obj any) uint64 {
	v := reflect.ValueOf(obj)
	addr := v.Pointer()
	if t, ok := addrTokens.Load(addr); ok {
		return t.(uint64)
	}
	tok := atomic.AddUint64(&globalTokenSeq, 1)
	actual, loaded := addrTokens.LoadOrStore(addr, tok)
	if loaded {
		return actual.(uint64)
	}
	runtime.SetFinalizer(obj, func(any) {
		// Only delete if the token still belongs to this generation.
		if cur, ok := addrTokens.Load(addr); ok && cur == tok {
			addrTokens.Delete(addr)
		}
	})
	return tok
}

func (it *identityTokens) get(obj any) (string, bool) {
	it.mu.Lock()
	defer it.mu.Unlock()
	addr := reflect.ValueOf(obj).Pointer()
	tok, seen := it.m[addr]
	if !seen {
		return "", false
	}
	// Address reuse after GC: only accept the token if the live object
	// still owns that generation globally.
	if cur, ok := addrTokens.Load(addr); !ok || cur != tok {
		delete(it.m, addr)
		return "", false
	}
	sid, ok := it.known[tok]
	return sid, ok
}

func (it *identityTokens) put(obj any, sid string) {
	it.mu.Lock()
	defer it.mu.Unlock()
	addr := reflect.ValueOf(obj).Pointer()
	tok := tokenOf(obj)
	it.m[addr] = tok
	it.known[tok] = sid
}
