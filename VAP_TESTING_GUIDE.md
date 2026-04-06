# Quick Reference - Testing VAP Memory Optimizations

## Current Cluster Status

**Cluster Name**: `kind-kind-vap-patch`  
**Status**: ✅ Running  
**Custom Image**: `custom-k8s:vap-patch`

---

## Verify Patches Are Active

### Option 1: View Kube-APIServer Logs (Easiest)
```bash
kubectl logs -n kube-system -l component=kube-apiserver --context kind-kind-vap-patch --tail=50 | grep -E "(CUSTOM|PATCH|VAP)"
```

### Option 2: View Full Container Logs
```bash
docker exec kind-vap-patch-control-plane bash -c 'cat /var/log/pods/kube-system_kube-apiserver-kind-vap-patch-control-plane_*/kube-apiserver/0.log' | grep "CUSTOM PATCH"
```

### Expected Output
```
I0406 11:09:52.806301       1 apiserver.go:33] CUSTOM PATCH APPLIED: Starting kube-apiserver with VAP optimizations
I0406 11:09:52.887570       1 plugin.go:82] CUSTOM PATCH: VAP Plugin initialized with memory optimizations
```

---

## Test VAP with Sample Policy

### 1. Create a ValidatingAdmissionPolicy
```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: test-vap-policy
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      operations: ["CREATE"]
      resources: ["pods"]
    namespaceSelector:
      matchLabels:
        admission-test: enabled
    objectSelector: {}
  validations:
  - expression: "object.metadata.name != 'forbidden-pod'"
    message: "Pod name 'forbidden-pod' is not allowed"
```

### 2. Create a Binding
```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: test-binding
spec:
  policyName: test-vap-policy
  validationActions:
  - deny
  matchResources:
    namespaceSelector: {}
    resourceRules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      operations: ["CREATE"]
      resources: ["pods"]
```

### 3. Test the Policy
```bash
# Create a namespace with the label
kubectl create ns test-vap
kubectl label ns test-vap admission-test=enabled

# This should succeed
kubectl run allowed-pod --image=nginx -n test-vap

# This should be denied
kubectl run forbidden-pod --image=nginx -n test-vap
# Expected error: ValidatingAdmissionPolicy 'test-vap-policy' with binding 'test-binding' denied request: Pod name 'forbidden-pod' is not allowed
```

---

## Monitor Performance

### Check API Server Resource Usage
```bash
kubectl top pod -n kube-system -l component=kube-apiserver --context kind-kind-vap-patch
```

### Monitor CEL Expression Evaluation
```bash
# Watch for compilation logs
kubectl logs -n kube-system -l component=kube-apiserver --context kind-kind-vap-patch -f | grep -E "(Compiling|PATCH_LOG)"
```

---

## Cluster Operations

### View Cluster Info
```bash
kubectl cluster-info --context kind-kind-vap-patch
```

### Get Nodes
```bash
kubectl get nodes --context kind-kind-vap-patch -o wide
```

### Check All System Pods
```bash
kubectl get pods -n kube-system --context kind-kind-vap-patch
```

### Delete Cluster (when done)
```bash
kind delete cluster --name kind-vap-patch
```

---

## Files Modified for Testing

1. `/cmd/kube-apiserver/app/server.go` - Added startup logging
2. `/staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go` - Added VAP plugin logging
3. `/Dockerfile.custom` - Created custom Docker image with patched binary

---

## Build Details

- **kube-apiserver Binary**: `/Users/md007711/Desktop/dev-projects/kubernetes/_output/bin/kube-apiserver` (78MB)
- **Docker Image**: `custom-k8s:vap-patch`
- **Kind Config**: `/tmp/kind-config.yaml`

---

## Log Files

### Container Logs
```
/var/log/pods/kube-system_kube-apiserver-kind-vap-patch-control-plane_*/kube-apiserver/0.log
```

### View via Docker
```bash
docker exec kind-vap-patch-control-plane bash -c 'ls -la /var/log/pods/kube-system_*/'
```

---

## Performance Testing Commands

### Create Test Namespace
```bash
kubectl create ns perf-test
kubectl label ns perf-test admission-test=enabled
```

### Create Multiple Pods to Test CEL Evaluation
```bash
for i in {1..10}; do
  kubectl run test-pod-$i --image=nginx -n perf-test &
done
wait
```

### Monitor Admission Metrics
```bash
kubectl get --raw /metrics | grep admission
```

---

## Troubleshooting

### If patches aren't showing in logs:
1. Check if container is running: `docker ps | grep kind-vap-patch`
2. Verify image: `docker image ls | grep custom-k8s`
3. Check logs directly: `docker logs kind-vap-patch-control-plane | head -200`

### Reset Cluster
```bash
kind delete cluster --name kind-vap-patch
kind create cluster --config /tmp/kind-config.yaml
```

### Rebuild Image
```bash
cd /Users/md007711/Desktop/dev-projects/kubernetes
make clean
make all WHAT=cmd/kube-apiserver
docker build -f Dockerfile.custom -t custom-k8s:vap-patch .
```
