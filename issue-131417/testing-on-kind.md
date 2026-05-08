# Testing the patches on a kind cluster

Three workflows for putting your patched `kube-apiserver` (or any
control-plane binary) onto a running cluster, ranked by iteration
speed. They all work without internet *after a one-time base-image
prefetch* — that's covered in §4.

You're on `darwin/arm64` (Apple M2) with `kind v0.31.0` installed.
Kind containers run as native `linux/arm64` on M2, so cross-compile
target is `linux/arm64`. If your kind nodes are amd64, swap
`arm64` → `amd64` everywhere.

---

## TL;DR — fastest iteration loop for one binary

```bash
# 1. Cross-compile the apiserver for the kind node arch (~1-2 min):
cd /Users/mukuldhariwal/Desktop/github/kubernetes
make WHAT=cmd/kube-apiserver KUBE_BUILD_PLATFORMS=linux/arm64

# 2. Find the node container and copy the binary in:
NODE=$(docker ps --filter name=control-plane --format '{{.Names}}' | head -1)
docker cp _output/local/bin/linux/arm64/kube-apiserver \
    "$NODE":/usr/local/bin/kube-apiserver-patched

# 3. Patch the static pod manifest to use the new binary
#    (lives at /etc/kubernetes/manifests/kube-apiserver.yaml inside the node)
docker exec "$NODE" sed -i \
    's|/usr/local/bin/kube-apiserver|/usr/local/bin/kube-apiserver-patched|g' \
    /etc/kubernetes/manifests/kube-apiserver.yaml

# 4. kubelet sees the manifest mtime change and restarts the static pod.
#    Watch it come back up:
docker exec "$NODE" crictl ps | grep kube-apiserver
kubectl --context kind-$(echo "$NODE" | sed 's/-control-plane//') get nodes
```

Iteration time after the first build: **~10 seconds** (binary copy +
kubelet pickup). The rest of this doc explains why each step exists
and what the alternatives are.

---

## 1. The hack/ scripts you asked about

The repo has 113 scripts in `hack/`. The ones relevant to running a
cluster:

| Script | What it does | Useful for |
|---|---|---|
| [hack/local-up-cluster.sh](../hack/local-up-cluster.sh) | Spins up a single-node cluster on **your host**, no Docker, no kind. Runs etcd + kube-apiserver + kube-controller-manager + kube-scheduler + kubelet as plain processes. | Fastest possible iteration; great for CEL admission testing because you don't need to rebuild images at all. Kubelet runs as root so needs `sudo`. |
| [hack/dev-build-and-up.sh](../hack/dev-build-and-up.sh) | `make quick-release` then `cluster/kube-up.sh` (the legacy GCE/AWS bring-up). | Mostly historical; not the right tool for kind. |
| [hack/dev-build-and-push.sh](../hack/dev-build-and-push.sh) | Same as above but pushes binaries to a remote dev cluster. | Same. |
| [hack/get-build.sh](../hack/get-build.sh) | Downloads a pre-built release. Not what you want. | n/a |
| [hack/install-etcd.sh](../hack/install-etcd.sh) | Installs etcd into `third_party/etcd/`. Used by `local-up-cluster.sh` and integration tests. | Run once before `local-up-cluster.sh`. |
| `hack/make-rules/build.sh` | The actual build driver invoked by `make all WHAT=...`. | Don't call directly; use `make`. |

For your case (kind + iterative apiserver patching) **none of these
are the right tool**. The right primitives are:

- `make WHAT=cmd/kube-apiserver KUBE_BUILD_PLATFORMS=linux/arm64`
  to build the binary
- `kind` itself for cluster lifecycle
- `docker cp` / `docker exec` for in-place binary swapping

`local-up-cluster.sh` *is* a strong alternative if you don't need a
real container runtime — see §5.

---

## 2. The image build pipeline

`build/root/Makefile` exposes:

| Target | What it builds |
|---|---|
| `make` / `make all` / `make all WHAT=cmd/kube-apiserver` | Binaries only, in `_output/local/bin/<host-os>/<host-arch>/`. |
| `make all WHAT=cmd/kube-apiserver KUBE_BUILD_PLATFORMS=linux/arm64` | Cross-compile binary for a specific platform (what you want for kind). |
| `make quick-release` | All control-plane + node binaries for all release platforms, no tests. |
| `make quick-release-images` | Same as `quick-release` plus build the per-binary docker images via `docker buildx`. Output: `_output/release-images/<arch>/{kube-apiserver,kube-controller-manager,kube-scheduler,kube-proxy}.tar`. |
| `make release-images` | Same as `quick-release-images` but also runs tests and builds the conformance image. |

The image Dockerfile is small — see
[`build/server-image/kube-apiserver/Dockerfile`](../build/server-image/kube-apiserver/Dockerfile).
It uses two base images:

```
ARG BASEIMAGE         # registry.k8s.io/build-image/debian-base:<version>
ARG SETCAP_IMAGE      # registry.k8s.io/build-image/setcap:<version>
```

These are the **only** network dependencies of the image build. Once
they're cached locally (§4) the rest is fully offline.

---

## 3. Three workflows for kind, ranked by speed

### Workflow A — Static-pod binary swap (10s iteration, recommended)

This is the TL;DR above, expanded.

The kind node image (`kindest/node:vX.Y.Z`) bakes the kube-apiserver
binary at `/usr/local/bin/kube-apiserver` and ships a static pod
manifest at `/etc/kubernetes/manifests/kube-apiserver.yaml`. The
manifest references `image: registry.k8s.io/kube-apiserver:vX.Y.Z`
and that image is **pre-loaded into the node's containerd** (no
internet pull needed). The static pod's `command:` is
`/usr/local/bin/kube-apiserver`, NOT pulled from the image — kubelet
joins the host filesystem of the node into the pod.

So to swap the binary you only need to:

```bash
# Build for linux/arm64 (kind on M2):
make WHAT=cmd/kube-apiserver KUBE_BUILD_PLATFORMS=linux/arm64

# Copy into the node container's filesystem:
NODE=kind-control-plane
docker cp _output/local/bin/linux/arm64/kube-apiserver \
    "$NODE":/usr/local/bin/kube-apiserver-patched

# Either edit the static pod manifest to point at the new path:
docker exec "$NODE" sed -i \
    's|/usr/local/bin/kube-apiserver|/usr/local/bin/kube-apiserver-patched|' \
    /etc/kubernetes/manifests/kube-apiserver.yaml
# (kubelet detects the manifest change and restarts the pod within ~5s)

# Or overwrite the original (kubelet won't restart on identical manifest,
# but the static pod will use the new binary on its next restart cycle —
# trigger one with `crictl rm`):
docker cp _output/local/bin/linux/arm64/kube-apiserver \
    "$NODE":/usr/local/bin/kube-apiserver
docker exec "$NODE" crictl ps -a --name kube-apiserver -q | head -1 \
    | xargs -r docker exec "$NODE" crictl rm -f
```

To iterate a second time, just rebuild and re-`docker cp`.

**No image build, no docker buildx, no internet, no kind reload.**
The only requirement is that the binary you build matches the node's
CPU arch.

### Workflow B — Full image build + `kind load`

When you want a fully-baked image (e.g. to share with another
machine, or to test the image itself, or because you've changed the
Dockerfile too):

```bash
# Cross-compile + image build for arm64:
KUBE_BUILD_PLATFORMS=linux/arm64 make quick-release-images

# Output:
ls _output/release-images/arm64/
# kube-apiserver.tar
# kube-controller-manager.tar
# kube-scheduler.tar
# kube-proxy.tar

# Load the apiserver image into the running kind cluster's containerd:
kind load image-archive _output/release-images/arm64/kube-apiserver.tar \
    --name <your-cluster-name>

# The image is loaded with its embedded tag, e.g.
# registry.k8s.io/kube-apiserver-arm64:v1.34.0-dirty
# Patch the static pod manifest to use it:
NODE=<your-cluster-name>-control-plane
docker exec "$NODE" sed -i \
    "s|registry.k8s.io/kube-apiserver:.*|registry.k8s.io/kube-apiserver-arm64:$(cat _output/release-images/arm64/kube-apiserver.docker_tag)|" \
    /etc/kubernetes/manifests/kube-apiserver.yaml
```

`kind load image-archive` is the offline-friendly path. It uses
`docker save | ctr -n=k8s.io image import` under the hood — no
network involved on the kind side.

### Workflow C — `kind build node-image` (clean rebuild, slowest)

For a fully-baked custom kindest/node with all four control-plane
binaries replaced:

```bash
kind build node-image /Users/mukuldhariwal/Desktop/github/kubernetes \
    --image kindest/node:patched

kind delete cluster --name patched
kind create cluster --name patched --image kindest/node:patched
```

`kind build node-image` internally runs `make quick-release-images`
inside its own builder container, then bundles everything into one
kindest/node image. **It needs the same base images as
`make quick-release-images`** — pre-cache as in §4.

This is the canonical "test a custom k8s build with kind" path used
by SIG release / SIG cluster-lifecycle CI. Iteration time: ~10-15
min for a full build. Use it when Workflow A or B has gotten you in
a weird state and you want to start clean.

---

## 4. Going offline

The two pieces that touch the network in a normal `make
quick-release-images`:

1. `BASEIMAGE` (`registry.k8s.io/build-image/debian-base:<ver>`) —
   the runtime base image for the kube-apiserver image.
2. `SETCAP_IMAGE` (`registry.k8s.io/build-image/setcap:<ver>`) —
   used by the multi-stage build to apply
   `cap_net_bind_service` on the binary.

Pin them once with internet:

```bash
# One-time: pre-pull the base images. The exact tags are pinned in
# build/dependencies.yaml (search for "debian-base" and "setcap"):
grep -A1 'debian-base\|setcap' build/dependencies.yaml | head -20

# Then:
docker pull registry.k8s.io/build-image/debian-base:bookworm-v1.0.6
docker pull registry.k8s.io/build-image/setcap:bookworm-v1.0.6

# Save them in case Docker prunes:
docker save -o ~/k8s-baseimages.tar \
    registry.k8s.io/build-image/debian-base:bookworm-v1.0.6 \
    registry.k8s.io/build-image/setcap:bookworm-v1.0.6
# Then later, offline:
docker load -i ~/k8s-baseimages.tar
```

For the kind node image (`kindest/node`), the `kind build node-image`
command similarly needs the upstream `kindest/base` image — pre-pull
that once too.

After this one-time setup, **all three workflows in §3 work fully
offline.** Workflow A in particular doesn't even need a Docker
daemon to be online — it's just `docker cp` between local objects.

The build itself doesn't fetch anything else: Go modules are already
vendored under `vendor/`, the `make` build runs entirely against
that vendor tree.

---

## 5. Alternative: `hack/local-up-cluster.sh` — no Docker, no kind

If you don't actually need containers — and for testing CEL
admission policy memory you usually don't — `local-up-cluster.sh`
is the fastest debug loop that exists in this repo. It runs:

- a local etcd from `third_party/etcd/`
- kube-apiserver as a plain process
- kube-controller-manager + kube-scheduler as plain processes
- (optionally) a kubelet

…all directly on your Mac, all from `_output/local/bin/...`. No
container build, no image, no kind, no static pod manifests.

```bash
# One-time: fetch etcd
hack/install-etcd.sh
export PATH="${PWD}/third_party/etcd:${PATH}"

# Build the binaries for darwin/arm64 (your host):
make WHAT=cmd/kube-apiserver
make WHAT=cmd/kube-controller-manager
make WHAT=cmd/kube-scheduler

# Bring it up. By default it runs on http://localhost:6443.
# Skip kubelet (you don't need it for admission-policy memory testing):
ENABLE_DAEMON=true \
DENY_SECURITY_CONTEXT_ADMISSION="" \
hack/local-up-cluster.sh -O

# In another shell:
export KUBECONFIG=/var/run/kubernetes/admin.kubeconfig
kubectl get --raw /healthz
kubectl apply -f my-vap.yaml
```

Memory of the running kube-apiserver process is observable via:

```bash
pid=$(pgrep -f 'kube-apiserver --')
ps -o rss= -p "$pid"          # resident set
curl -s localhost:6443/debug/pprof/heap > /tmp/heap.pprof
go tool pprof -top -inuse_space /tmp/heap.pprof
```

This is exactly how the
[heap profiles in `analysis/master-analysis.md`](analysis/master-analysis.md)
were captured. For testing the issue-131417 patches end-to-end this
is by far the cleanest setup — no kind weirdness, just your build.

The main limitation: scheduler/controller-manager aren't strictly
necessary for VAP admission tests, and pod scheduling won't work
without a kubelet. If you only need to admit and reject objects via
CEL policies (no actual pod runtime), this is perfect.

---

## 6. Sanity check — confirming your patch is live

Once the apiserver is up with your custom binary:

```bash
# Confirm it's your binary by build timestamp / git commit:
docker exec <node> /usr/local/bin/kube-apiserver --version
# v1.34.0-dirty

# For changes that touched the cel package, confirm by hitting an
# endpoint that exercises it. E.g. for patch 0010 (lazy varEnvs):
kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: test-policy
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      operations: ["CREATE"]
      resources: ["configmaps"]
  validations:
  - expression: "true"
EOF

# Heap snapshot before vs after creating N policies confirms
# the per-policy retained delta.
curl -s localhost:6443/debug/pprof/heap > /tmp/before.pprof
for i in $(seq 1 100); do
    sed "s/test-policy/test-policy-$i/" my-vap.yaml | kubectl apply -f -
done
sleep 5  # wait for refreshPolicies tick
curl -s localhost:6443/debug/pprof/heap > /tmp/after.pprof
go tool pprof -inuse_space -base /tmp/before.pprof /tmp/after.pprof
# top
```

For kind specifically, port-forward `/debug/pprof` if it's not
exposed by default:

```bash
kubectl --context kind-<cluster> port-forward -n kube-system \
    pod/kube-apiserver-<node> 6443:6443 &
curl -sk https://localhost:6443/debug/pprof/heap > /tmp/heap.pprof
```

---

## Recommendation matrix

| Goal | Use |
|---|---|
| Iterate quickly on apiserver code, see effect on a real-ish cluster | **Workflow A** (binary swap) |
| Heap-profile the patches in isolation, no cluster machinery | **§5 `local-up-cluster.sh`** |
| Test the full image build path / Dockerfile changes | Workflow B |
| Reproduce a clean cluster from scratch with the patched build | Workflow C (`kind build node-image`) |
| Run e2e / conformance against the patched build | Workflow C + `hack/ginkgo-e2e.sh` |
| Test on a node arch ≠ host arch | Workflow B with explicit `KUBE_BUILD_PLATFORMS` |

For your current goal (validate that patches 0010 / 0011 / 0012 do
what they claim on a real apiserver heap), **§5 is what I'd do
first**, then Workflow A if you also want to see admission events
flow through.
