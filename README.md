# Horizon

A Kubernetes autoscaler that **forecasts** near-future workload — request rate and queue depth, not just CPU/RAM — and proactively adjusts pod replica counts, instead of relying purely on reactive threshold-based scaling like the default Horizontal Pod Autoscaler (HPA).

Every scaling decision is bounded by a hybrid safety rule:

```
target_replicas = max(predicted_replicas, reactive_replicas)
```

A wrong forecast can only ever fall back to what plain reactive scaling would have done — it can never make things worse than standard HPA behavior.

---

## Why this exists

Reactive autoscaling only reacts *after* load has already increased, which means real user-facing latency spikes before the system catches up. Predictive scaling tries to act ahead of the spike — but naive implementations fail in production for well-understood reasons: forecast lead time shorter than pod boot time, treating over- and under-provisioning as equally bad, and blindness to unannounced traffic spikes. This project is built around explicitly addressing those failure modes rather than ignoring them.

**Status:** 🚧 Actively under development. See [Roadmap](#roadmap) for current phase.

---

## Architecture

```
Terraform               → provisions AWS infrastructure (EKS cluster, node groups)
AWS EKS / local kind    → Kubernetes control plane
Kubernetes              → schedules, scales, heals containers
Docker                  → packages the sample app as a container image
Sample app              → minimal API used as the scaling target
Prometheus / Grafana    → metrics collection + visualization
Load generator          → seasonal synthetic traffic OR replayed historical trace
Decision Engine         → Prometheus metrics → forecast → max(predicted, reactive) → replica count
Actuator                → applies the replica count to the cluster (later: KEDA External Scaler)
Decision Visualizer     → shows *why* a scaling decision was made, not just cluster state
```

### Design principles

1. **Predictive never overrides safety.** The `max(predicted, reactive)` rule is implemented from the very first working version — not treated as a later hardening step.
2. **Decision Engine and Actuator are strictly separated.** The Decision Engine is a pure function (metrics in, replica count out) that must be unit-testable with zero live cluster involved. The Actuator is a thin, swappable wrapper that applies that number. This means moving to a proper KEDA External Scaler later only requires rewriting the actuator, not the decision logic.
3. **Workload-aware forecasting, not lagging indicators.** Forecasts target request rate / queue depth rather than CPU or memory, which lag the traffic that actually caused them.

---

## Known failure modes this project is designed around

| Failure mode | Problem | Mitigation |
|---|---|---|
| Cold start vs. lead time | A forecast that arrives after pod boot time has already passed is useless | Forecast horizon is chosen to comfortably exceed provision + boot + warmup time for the sample app |
| Asymmetric error cost | Under-provisioning causes real latency/SLA impact; over-provisioning only wastes spend — plain MSE loss treats both equally | Hybrid `max(predicted, reactive)` floor from day one; quantile/conformal prediction planned to bias toward safety |
| Black-swan spikes | Seasonality-based forecasting models (Prophet/Holt-Winters) cannot anticipate unannounced spikes | Reactive fallback in the `max()` rule catches whatever the model misses |
| Fighting the platform | A standalone daemon patching replicas directly can oscillate against Kubernetes' own control loops | Decision Engine / Actuator separation is designed to allow migration to a proper KEDA External Scaler |

---

## Roadmap

- [ ] **Phase 1** — Sample app containerized and deployed to a local Kubernetes cluster (`kind`)
- [ ] **Phase 2** — Prometheus + Grafana monitoring via `kube-prometheus-stack`
- [ ] **Phase 3** — Load generation (synthetic seasonal pattern or replayed historical trace)
- [ ] **Phase 4** — Decision Engine: forecasting + `max(predicted, reactive)`, fully unit-tested standalone
- [ ] **Phase 5** — Actuator: applies Decision Engine output to the live cluster (dry-run first)
- [ ] **Phase 5.5** — Decision-reasoning visualizer (predicted vs. actual, which side of `max()` won, replica lag)
- [ ] **Phase 6** — Terraform-provisioned AWS EKS cluster, migrated off local `kind`
- [ ] **Phase 7** — Quantile/conformal prediction replacing point-estimate forecasts
- [ ] **Phase 8** (stretch) — KEDA External Scaler: Decision Engine exposed via gRPC as a proper Kubernetes-native metric provider

---

## Tech stack

| Component | Tooling |
|---|---|
| Sample app + Actuator | Go |
| Decision Engine (forecasting) | Python |
| Container runtime | Docker |
| Local cluster | kind |
| Cloud cluster | AWS EKS |
| Infrastructure as code | Terraform |
| Metrics | Prometheus + Grafana (`kube-prometheus-stack`) |
| Load testing | k6 / Locust, replaying real historical traces (NASA-HTTP, Alibaba Cloud) |
| Forecasting | Prophet / Holt-Winters → quantile regression / conformal prediction |
| Actuator (v1) | `client-go` (Go) |
| Actuator (v2, stretch) | Custom KEDA `ExternalScaler` via gRPC |

**Go ↔ Python boundary:** since the sample app/actuator is Go and the Decision Engine is Python, the interface between them (e.g. the Python service exposing decisions over a small internal HTTP/gRPC endpoint the Go actuator polls) needs to be designed explicitly in Phase 4, rather than left implicit.

---

## Getting started

> This section will be filled in as each phase lands. For now:

```bash
# Clone
git clone <repo-url>
cd horizon

# Local cluster (Phase 1)
kind create cluster --name horizon
kubectl apply -f manifests/
```
