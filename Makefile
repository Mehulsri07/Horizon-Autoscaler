# Horizon Lab — local kind lab for the LoadLab backend
#
#   make cluster-up    Create the kind cluster and load the backend images
#   make deploy        Apply the manifests and wait for the rollout
#   make validate      Validate the manifests offline with kubeconform
#   make cluster-down  Delete the kind cluster

CLUSTER := horizon-lab
IMAGES  := loadlab-backend:local loadlab-backend:bad

.PHONY: cluster-up deploy validate cluster-down

cluster-up:
	kind create cluster --config deploy/kind/cluster.yaml
	kind load docker-image $(IMAGES) --name $(CLUSTER)

# The namespace goes first: a directory apply runs files alphabetically.
deploy:
	kubectl apply -f deploy/loadlab/namespace.yaml
	kubectl apply -f deploy/loadlab/
	kubectl -n loadlab rollout status deploy/loadlab-backend --timeout=120s

validate:
	docker run --rm -v "$(CURDIR)/deploy:/d" ghcr.io/yannh/kubeconform:v0.6.7 -strict -summary -kubernetes-version 1.32.0 /d/loadlab

cluster-down:
	kind delete cluster --name $(CLUSTER)
