# 0003 — Expression normalization utility

**Status:** standalone utility, not wired by default.
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`).
**Headline impact:** lifts cache hit rate on 0001/0002 when policy authors
write the same expression with different incidental whitespace.

## What it does

Adds `cel.NormalizeExpression(string) string` — a deterministic, idempotent
canonicalizer that:

- strips leading and trailing whitespace,
- normalizes CRLF / standalone CR to LF,
- strips trailing whitespace from each internal line,
- preserves all internal whitespace (including inside string literals)
  — critical so cel-go error spans aren't perturbed.

## Why

Cache keys in 0001/0002 hash the expression text. Two policies with byte
sequences `"x == 1"` and `"x == 1\r\n"` (or with a trailing space) hash to
distinct keys and produce distinct compiled programs even though they are
semantically and syntactically identical. Normalization at the cache-key
boundary lifts the hit rate without changing observable behaviour for
non-cache callers.

## Files touched

Added:
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/normalize.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/normalize_test.go`

Modified: none (utility-only).

## Public API additions

- `NormalizeExpression(string) string`

## Tests added

10 case-driven tests + idempotency test + 2 benchmarks (canonical fast
path vs needs-normalization slow path).

## Wiring

Standalone the utility is a no-op on existing behaviour. Future patches
(or a follow-up to 0001/0002) can call it inside `computeCompileCacheKey`
or the composited-compiler cache key path.

## Apply

```
git apply patches/0003-expression-normalization/0003-cel-add-expression-normalization-utility.patch
```
