/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cel

import (
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/google/cel-go/cel"

	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/utils/lru"
)

// reflectPointer returns the numeric identity of the given pointer using
// reflect (no unsafe).
func reflectPointer(p *environment.EnvSet) uintptr {
	return reflect.ValueOf(p).Pointer()
}

// defaultCompileCacheSize bounds how many distinct compiled CEL programs are
// kept in the process-wide cache. Each entry holds one cel.Program (typically
// 30-200 KB), so 5000 entries cap the cache footprint at a few hundred MB
// even on pathological clusters.
const defaultCompileCacheSize = 5000

var (
	// globalCompileCache is swapped via atomic.Pointer so the hot path of
	// CompileCELExpression is lock-free.
	globalCompileCache atomic.Pointer[lru.Cache]

	compileCacheHits   atomic.Int64
	compileCacheMisses atomic.Int64
)

func init() {
	globalCompileCache.Store(lru.New(defaultCompileCacheSize))
}

// compileCache returns the current process-wide compile cache.
func compileCache() *lru.Cache {
	return globalCompileCache.Load()
}

// CompileCacheStats reports current stats from the global compile cache.
// Exposed for metrics wiring and tests.
type CompileCacheStats struct {
	Size   int
	Hits   int64
	Misses int64
}

// GetCompileCacheStats returns current cache stats.
func GetCompileCacheStats() CompileCacheStats {
	return CompileCacheStats{
		Size:   compileCache().Len(),
		Hits:   compileCacheHits.Load(),
		Misses: compileCacheMisses.Load(),
	}
}

// SetCompileCacheSizeForTests replaces the process-wide cache with one of the
// requested size and returns a function that restores the previous cache and
// counters. Test only.
func SetCompileCacheSizeForTests(size int) func() {
	prev := globalCompileCache.Load()
	prevHits := compileCacheHits.Load()
	prevMisses := compileCacheMisses.Load()
	globalCompileCache.Store(lru.New(size))
	compileCacheHits.Store(0)
	compileCacheMisses.Store(0)
	return func() {
		globalCompileCache.Store(prev)
		compileCacheHits.Store(prevHits)
		compileCacheMisses.Store(prevMisses)
	}
}

// compileCacheKey is a fixed-size structural hash of every input that affects
// a compiled CEL Program. Using a fixed-size byte array keeps the LRU map
// efficient.
type compileCacheKey [sha256.Size]byte

// computeCompileCacheKey produces a stable, collision-resistant key for the
// cache. The key incorporates every input that can change the compiled output:
//
//   - the env template identity (so compilers built from different env
//     templates — e.g. one with a custom cel.CostLimit — never share entries)
//   - normalized expression source (whitespace-trimmed)
//   - environment Type (NewExpressions vs StoredExpressions)
//   - OptionalVariableDeclarations (HasParams, HasAuthorizer, HasPatchTypes)
//   - sorted list of allowed return types from the ExpressionAccessor
//   - sorted "name:type" snapshot of in-scope composition variables
//
// All inputs are length-prefixed before hashing to avoid collisions caused by
// adjacent fields concatenating into the same byte sequence.
func computeCompileCacheKey(
	templateID *environment.EnvSet,
	expr string,
	envType environment.Type,
	opts OptionalVariableDeclarations,
	returnTypes []*cel.Type,
	varSig string,
) compileCacheKey {
	h := sha256.New()
	writeLP := func(b []byte) {
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(b)
	}

	// The templateID pointer disambiguates compilations performed with
	// different env templates (different program options, libraries, etc.).
	var idBuf [8]byte
	binary.BigEndian.PutUint64(idBuf[:], uint64(templateIDValue(templateID)))
	writeLP(idBuf[:])

	writeLP([]byte(normalizeExpression(expr)))
	writeLP([]byte(envType))

	var optsBits byte
	if opts.HasParams {
		optsBits |= 1
	}
	if opts.HasAuthorizer {
		optsBits |= 2
	}
	if opts.HasPatchTypes {
		optsBits |= 4
	}
	writeLP([]byte{optsBits})

	rts := make([]string, 0, len(returnTypes))
	for _, t := range returnTypes {
		if t == nil {
			rts = append(rts, "")
			continue
		}
		rts = append(rts, t.String())
	}
	sort.Strings(rts)
	for _, rt := range rts {
		writeLP([]byte(rt))
	}

	writeLP([]byte(varSig))

	var out compileCacheKey
	copy(out[:], h.Sum(nil))
	return out
}

// normalizeExpression performs conservative normalization on an expression
// string for the purposes of cache-key computation only. Internal whitespace
// is intentionally preserved because changing it could shift error spans
// surfaced by cel-go.
func normalizeExpression(expr string) string {
	return strings.TrimSpace(expr)
}

// recordCacheHit and recordCacheMiss are inlined-friendly counter bumps used
// by compiler.CompileCELExpression. They are exposed via GetCompileCacheStats.
func recordCacheHit()  { compileCacheHits.Add(1) }
func recordCacheMiss() { compileCacheMisses.Add(1) }

// templateIDValue extracts a numeric identity for a *environment.EnvSet pointer
// without importing unsafe. Two equal pointers yield equal values; nil yields 0.
func templateIDValue(p *environment.EnvSet) uintptr {
	if p == nil {
		return 0
	}
	// reflect.ValueOf(p).Pointer() is the standard, unsafe-free way to obtain
	// a pointer's numeric identity.
	return reflectPointer(p)
}
