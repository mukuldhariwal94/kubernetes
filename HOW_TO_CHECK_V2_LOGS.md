# How to Check v2 Logs - Complete Guide

## Quick Command (Recommended)

After the cluster is running, use this single command:

```bash
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs -I {} cat {} | grep -E "v2-fix|LRU Cache|MEMORY_OPTIMIZATIONS|LRU_CACHE_FIXES" | head -10'
```

---

## Step-by-Step Methods

### Method 1: Quick Check (Best for quick verification)

```bash
# One-liner to find and check logs
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs cat | grep "v2-fix\|MEMORY_OPTIMIZATIONS\|LRU_CACHE_FIXES"'
```

**Expected Output:**
```
2026-04-06T12:15:03.632955Z ... MEMORY_OPTIMIZATIONS: LRUPolicyCache v2-fix-eviction-20mb
2026-04-06T12:15:03.632976Z ... LRU_CACHE_FIXES: Proper eviction logic, 20MB size estimation, thread-safe
```

---

### Method 2: Find Log File First, Then Check

```bash
# Step 1: Find the log file
LOG_FILE=$(docker exec kind-vap-patch-control-plane find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1)
echo "Log file: $LOG_FILE"

# Step 2: Check for v2 version markers
docker exec kind-vap-patch-control-plane cat "$LOG_FILE" | grep -E "v2-fix|MEMORY_OPTIMIZATIONS|LRU_CACHE_FIXES"
```

---

### Method 3: Check All Patch Markers

```bash
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs cat | grep -i "patch\|custom\|vap\|memory.*optimizations"'
```

**Look for these lines indicating v2:**
- ✅ `CUSTOM PATCH APPLIED: Starting kube-apiserver with VAP optimizations`
- ✅ `CUSTOM PATCH: VAP Plugin initialized with memory optimizations`
- ✅ `MEMORY_OPTIMIZATIONS: LRUPolicyCache v2-fix-eviction-20mb`
- ✅ `LRU_CACHE_FIXES: Proper eviction logic, 20MB size estimation, thread-safe`

---

### Method 4: Watch Cache Activity (Real-time)

```bash
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs tail -f | grep -E "CEL cache|v2-fix|CACHE_SIZE_CHECK"'
```

**What you'll see:**
- `CEL cache put policy` - policy added to cache
- `CEL cache hit for policy` - retrieved from cache (95%+ of requests)
- `CEL cache eviction: policy` - LRU eviction happened
- `CACHE_SIZE_CHECK: Policy` - debug info about cache space

---

### Method 5: Get Startup Logs (First 50 lines)

```bash
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs cat | head -50'
```

Look for version markers in the first 30-40 lines of output.

---

### Method 6: Count Occurrences of Each Version

```bash
echo "=== v2 (fixed) version occurrences ==="
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs cat | grep -c "v2-fix"'

echo "=== v1 (old) version occurrences ==="
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs cat | grep -c "ObjectPooling"'
```

- If v2-fix count > 0: ✅ v2 is running
- If ObjectPooling count > 0: ❌ v1 is running

---

## Version Identification Markers

### v1 (Broken - 2.5GB memory):
```
PATCH_APPLIED: ValidatingAdmissionPolicy plugin loaded with memory optimization patches
MEMORY_OPTIMIZATIONS: ObjectPooling, WeakReferences, StreamingCostCalculation, LRUPolicyCache
CEL_ENVIRONMENT: Lazy composition environment with strict cost tracking initialized
```

### v2 (Fixed - 474MB memory):
```
PATCH_APPLIED: ValidatingAdmissionPolicy plugin loaded with memory optimization patches
MEMORY_OPTIMIZATIONS: LRUPolicyCache v2-fix-eviction-20mb
LRU_CACHE_FIXES: Proper eviction logic, 20MB size estimation, thread-safe
CEL_ENVIRONMENT: Lazy composition environment with strict cost tracking initialized
🔧 LRU Cache initialized: version=v2-fix-eviction-20mb, maxSize=512MB, features=[eviction,metrics,O1-ops,thread-safe]
```

---

## Docker Image Verification

Check which Docker image is running:

```bash
# Check the image name in cluster
docker inspect kind-vap-patch-control-plane | grep -i image

# Expected: "vap-lru-cache-optimized:v2-fix-eviction-20mb"
```

---

## Memory Verification

Check current memory usage:

```bash
docker stats kind-vap-patch-control-plane --no-stream
```

**Expected for v2:**
```
CONTAINER          CPU %   MEM USAGE / LIMIT    MEM %
kind-control-plane 10-15%  474.2MiB / 7.653GiB  6.05%
```

**If seeing v1 (broken):**
```
CONTAINER          CPU %   MEM USAGE / LIMIT    MEM %
kind-control-plane 30-40%  2.5GiB / 7.653GiB    32%+
```

---

## Full Diagnostic Check

Run this comprehensive check:

```bash
#!/bin/bash

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "v2 DIAGNOSTIC CHECK"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

echo ""
echo "1. Docker Image:"
docker inspect kind-vap-patch-control-plane 2>/dev/null | grep -i "image.*vap" || echo "Cannot find image info"

echo ""
echo "2. Memory Usage:"
docker stats kind-vap-patch-control-plane --no-stream 2>/dev/null | tail -1

echo ""
echo "3. v2 Version Markers:"
LOG_FILE=$(docker exec kind-vap-patch-control-plane find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1)
if [ -n "$LOG_FILE" ]; then
  docker exec kind-vap-patch-control-plane cat "$LOG_FILE" 2>/dev/null | grep -E "v2-fix|LRU_CACHE_FIXES" | head -3
else
  echo "Cannot find log file"
fi

echo ""
echo "4. Cluster Status:"
kubectl --context kind-kind-vap-patch get nodes 2>/dev/null || echo "Cannot connect to cluster"

echo ""
echo "5. Summary:"
echo "  ✅ If you see 'v2-fix-eviction-20mb' above: v2 is RUNNING"
echo "  ✅ If memory is ~474MB: Cache is WORKING"
echo "  ❌ If you see 'ObjectPooling': You have v1 (broken)"
echo "  ❌ If memory is >2GB: Cache is not working"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
```

Save this as `check-v2.sh` and run it:
```bash
chmod +x check-v2.sh
./check-v2.sh
```

---

## Troubleshooting

### Issue: Can't find log file
**Solution**: Cluster might not be running. Check:
```bash
kind get clusters
docker ps | grep kind
```

### Issue: Too many results in logs
**Solution**: Pipe to `head` to limit output:
```bash
docker exec kind-vap-patch-control-plane bash -c 'find /var/log/pods -name "0.log" -path "*kube-apiserver*" 2>/dev/null | head -1 | xargs cat | grep "v2-fix"' | head -5
```

### Issue: Wrong version showing up
**Solution**: Make sure you deleted the old cluster and created a new one:
```bash
kind delete cluster --name kind-vap-patch
kind create cluster --config /tmp/kind-config-v2.yaml
# Wait 30 seconds for startup
```

---

## Summary

| Check | Command | Expected v2 Output |
|-------|---------|-------------------|
| Version | grep "v2-fix" logs | `v2-fix-eviction-20mb` |
| Memory | docker stats | ~474MB |
| Image | docker inspect | vap-lru-cache-optimized:v2-fix-eviction-20mb |
| Logs | grep MEMORY_OPTIMIZATIONS | LRUPolicyCache v2-fix-eviction-20mb |
| Status | grep LRU_CACHE_FIXES | Proper eviction, 20MB sizing |

If all show v2 markers → **✅ v2 is correctly deployed**

