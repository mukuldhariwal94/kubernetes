# How to Build and Test the Custom Kubernetes Apiserver

Since `kind build node-image` can be slow or fail depending on the local environment, the most reliable and fastest way to test changes to the `kube-apiserver` in a local `kind` cluster is by mounting the compiled binary directly into the `kind` control-plane node.

Here are the exact steps we used to successfully deploy and verify the `v3-lazy-evaluation` changes.

## 1. Build the binary
First, compile the `kube-apiserver` for the Linux ARM64 architecture (since Docker Desktop on Mac M-series runs a Linux ARM64 VM):
```bash
make all WHAT=cmd/kube-apiserver KUBE_BUILD_PLATFORMS=linux/arm64
```
This will place the binary at `_output/local/bin/linux/arm64/kube-apiserver`.

## 2. Create the Kind Cluster Configuration
Create a file named `kind-config.yaml` that tells `kind` to mount your locally compiled binary into the container and update the static pod manifest to use it instead of the default binary.

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  # 1. Mount the local binary into the kind node
  extraMounts:
  - hostPath: /Users/md007711/Desktop/dev-projects/kubernetes/_output/local/bin/linux/arm64/kube-apiserver
    containerPath: /usr/local/bin/kube-apiserver-custom
  
  # 2. Patch the kube-apiserver static pod to use the mounted binary
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      extraVolumes:
      - name: custom-apiserver
        hostPath: /usr/local/bin/kube-apiserver-custom
        mountPath: /usr/local/bin/kube-apiserver
        readOnly: true
```

## 3. Create the Cluster
Create the cluster using the default name (or any name you prefer) and the configuration file:
```bash
kind create cluster --config kind-config.yaml
```

## 4. Verify the Deployment
Once the cluster is up and running, you can verify that the custom binary is being used and the `v3` logs are present.

First, find the container ID of the `kube-apiserver`:
```bash
docker exec kind-control-plane crictl ps | grep kube-apiserver
```

Then, check the logs of that container:
```bash
docker exec kind-control-plane crictl logs <CONTAINER_ID> | head -n 20
```

You should see the `v3-lazy-evaluation` logs:
```
I0407 07:01:47.860616       1 plugin.go:84] MEMORY_OPTIMIZATIONS: LRUPolicyCache v3-lazy-evaluation
I0407 07:01:47.860617       1 plugin.go:85] LRU_CACHE_FIXES: Lazy evaluation of policies, no longer storing all evaluators in memory
```
