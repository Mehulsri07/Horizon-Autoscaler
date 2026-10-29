# Horizon Lab: design spec

*2026-10-29 · status: draft for review*

One local lab that ties three projects into a single DevOps/AIOps story:
**LoadLab** puts a real service under load, **Horizon** scales it ahead of demand,
and **IntelliOps** catches and fixes the failures that no forecast can see coming.

This spec supersedes the phase roadmap in `README.md` from Phase 2 onward (see
[Relationship to the existing roadmap](#1-relationship-to-the-existing-roadmap)).
The Phase 1 requirements in `requirements.md` are carried over into the LoadLab
hardening work in section 3.

---

## 0. Goals

**Audience:** recruiters and interviewers for DevOps, SRE and AIOps roles.

**v1 is done when:**

1. `make up` brings the lab up on a clean Windows laptop (kind + Docker Desktop) in under 10 minutes.
2. A one-month shadow trial has run on a replayed real traffic trace, and the README shows,
   per day type (weekday / weekend), each model's forecast error and the model that was promoted.
3. The README shows the scaling benchmark: SLO-violation minutes and replica-minutes for
   reactive vs each predictive model, at two time-compression factors (section 12).
4. The chaos and spike results are written up, including where prediction loses.
5. A 2-minute recording shows Horizon getting ahead of a peak, a flash sale being pre-scaled,
   a DDoS being held at the cap, and IntelliOps catching a memory leak and a bad deploy.
6. Total spend is ₹0.

**Non-goals for v1:** a custom Kubernetes operator, an LSTM, a cloud deployment that costs money,
more than one scaled service.

---

## 1. Relationship to the existing roadmap

The README's principles stay. Three of its choices change.

| README roadmap | This spec | Why |
|---|---|---|
| `max(predicted, reactive)` from day one | **Kept.** KEDA takes the highest of all triggers | Same rule, enforced by the platform |
| Decision Engine and Actuator strictly separated | **Kept.** The forecaster only publishes metrics; KEDA is the actuator | Decision logic stays a pure, unit-testable function |
| Phase 1 sample app `horizon-app` | **Replaced by the LoadLab backend**, hardened (section 3). `app/` is removed; its history stays in git | LoadLab already has stress endpoints and gets the fault switches IntelliOps needs |
| Phase 5 Actuator in Go (`client-go`), KEDA as a Phase 8 stretch | **KEDA from day one** (Prometheus trigger). A gRPC External Scaler stays a later option | Avoids the "fighting the platform" failure mode the README itself lists |
| Phase 3 load: synthetic or replayed trace | **Replayed NASA-HTTP trace** for the trial; synthetic k6 only for scenarios | Real daily *and* weekly patterns |
| Phase 5.5 decision visualizer | **The Horizon Lab console** (section 10) | Same purpose, now also the incident console |
| Phase 6 AWS EKS via Terraform | **Dropped.** Optional Oracle Cloud Always Free server, provisioned with Terraform | EKS bills from the first hour; the budget is ₹0 |
| Phase 7 quantile / conformal prediction | **Moved into v1** (section 5) | It is the main defence against under-forecasting peaks |

---

## 2. Architecture

```
                         kind cluster "horizon-lab"
 ┌──────────────────────────────────────────────────────────────────────┐
 │  trace replayer / k6 ──► ingress-nginx ──► loadlab-backend (Deployment)│
 │                          │ rate limits        │ /metrics              │
 │                          │ /metrics           │ admin port (internal)  │
 │                          ▼                    ▼                       │
 │                      Prometheus (45 d retention, PVC)                 │
 │                     ▲        │         ▲                              │
 │   horizon-forecaster┘        │         └ recording rules → IntelliOps │
 │   (Python; publishes          ▼            metric names              │
 │    forecasts, exports   KEDA ScaledObject                             │
 │    Parquet daily)       triggers: forecast q90 · ingress rate · CPU   │
 │                          → HPA → replicas (min 2, max 12)             │
 │                                                                      │
 │  horizon-console (FastAPI + React)      IntelliOps (demo profile only)│
 │   reads Prometheus, KEDA, IntelliOps     restart-pod / rollback-deploy│
 └──────────────────────────────────────────────────────────────────────┘
```

| Component | Repo | Language | Writes to the cluster? |
|---|---|---|---|
| LoadLab backend | LoadLab | Go | no |
| ingress-nginx | upstream (kind manifest) | n/a | no |
| Trace replayer | Horizon (`traffic/`) | Python, drives k6 | no |
| horizon-forecaster | Horizon (`forecaster/`) | Python | **no**, read-only |
| KEDA ScaledObject + HPA | Horizon (`deploy/`) | YAML | yes, replicas only |
| horizon-console | Horizon (`console/`) | Python + TypeScript | yes, namespace-scoped (section 10) |
| IntelliOps overlay | IntelliOps fork | YAML | restart / rollback via its own RBAC |

---

## 3. Workload: LoadLab backend hardening

Review of `LoadLab/backend/main.go` at `917c7d1`. Each item states the problem it causes for Horizon.

| # | Problem today | Effect on the lab | Change |
|---|---|---|---|
| 1 | `http.ListenAndServe` with no SIGTERM handling | Every scale-down kills in-flight requests, which shows up as errors and corrupts the benchmark | `http.Server` + `signal.NotifyContext` + `Shutdown` with a 20 s drain; readiness turns false first |
| 2 | No server timeouts | A slowloris-style DDoS holds connections forever | `ReadHeaderTimeout 5s`, `ReadTimeout 10s`, `WriteTimeout 15s`, `IdleTimeout 60s`, `MaxHeaderBytes 64 KiB` |
| 3 | `/health` always returns ok; no separate readiness | Pods get traffic before they're ready, and keep getting it while draining | `/livez` (process alive) and `/readyz` (initialized and not draining). Carries over Phase 1 US-2 |
| 4 | Handlers ignore the request context | Work continues after the client has gone, which amplifies overload and DDoS | `/slow` and `/cpu` stop on `r.Context().Done()` |
| 5 | No steady "typical request" endpoint | Per-pod capacity can't be measured cleanly | Port `/work` from `horizon-app`: cost set by `WORK_CPU_MS` / `WORK_DURATION_MS`, bounded by a context timeout |
| 6 | Latency buckets `0.05, 0.1, 0.3, …` | p95 near the 200 ms SLO is interpolated between 100 and 300 ms, which is too coarse | Buckets `0.01 0.025 0.05 0.075 0.1 0.15 0.2 0.25 0.3 0.4 0.5 0.75 1 2 5` |
| 7 | No concurrency signal | Saturation is only visible through latency | Add a `http_requests_in_flight` gauge and a `build_info{version}` gauge (the version shows up in the bad-deploy incident) |
| 8 | `/status` probes Docker Compose hostnames | Breaks on Kubernetes | Remove it from the Kubernetes build, or make the targets configurable through environment variables |
| 9 | No fault switches | IntelliOps scenarios can't be triggered | **Admin listener on a separate port (`:9091`), never routed by ingress:** `POST /admin/leak?mb_per_min=`, `POST /admin/leak/stop`. The bad deploy is a separate image tag `:bad`, built with `FAULT_LATENCY_MS=300`, `FAULT_ERROR_RATE=0.1` |
| 10 | No tests | Changes to the backend aren't checked | Table tests for handlers, shutdown drain, timeouts and the fault switches being off by default (carried over from `app/main_test.go`) |
| 11 | `alpine` runtime, runs as root | Larger image, slower pull, weaker security | `gcr.io/distroless/static:nonroot`, `-trimpath -ldflags "-s -w"`. Target from Phase 1 US-4: healthy within 5 s of container start |
| 12 | Compose file hard-codes the old EC2 IP `43.204.103.85` | Stale config | Remove it (compose stays for the standalone EC2-style demo) |

The Kubernetes manifests set `requests: cpu 250m, memory 64Mi` and `limits: cpu 1, memory 128Mi`,
`terminationGracePeriodSeconds: 30`, and a `preStop` sleep of 5 s so ingress stops routing to a pod before it drains.

---

## 4. Traffic

### 4.1 The trial: replayed NASA-HTTP trace

- **Source:** the NASA Kennedy Space Center HTTP logs, July and August 1995, from the Internet Traffic Archive.
  About two months of real requests with daily and weekly cycles.
- **Preprocessing** (`traffic/prepare_nasa.py`, run once, output committed as Parquet):
  1. Count requests per 15 s bucket of trace time.
  2. Mark the known outage gap in early August as missing. Verify the exact window from the data;
     don't interpolate over it.
  3. Rescale so the trace's 99th-percentile rate equals **6 replicas' worth** of measured per-pod capacity.
     That leaves room under the cap of 12 for the flash-sale and spike scenarios.
  4. Label every bucket with `day_type` (weekday / weekend) and `is_holiday` (4 July).
- **Replay:** the replayer feeds per-bucket target rates to a k6 `ramping-arrival-rate` scenario.
  It hits `/work` through ingress, so the offered load is what the trace says, independent of how scaling goes.
- **Resumable:** the replayer stores the last finished trace bucket, so the trial can run across
  several sessions (section 13).

### 4.2 Time compression

| Profile | 1 trace day = | One month takes | Used for |
|---|---|---|---|
| `fast` | 60 min | ~30 h | the shadow trial, demos |
| `honest` | 6 h | not run in full | the benchmark's second compression factor (3 trace days = 18 h) |

Pod start-up time is real time, so ramps are 24× steeper relative to pod start-up in `fast`.
That flatters prediction. Every published number states its compression factor.

### 4.3 Scenarios (synthetic k6, run on top of or instead of the trace)

| Scenario | Shape | Expected result |
|---|---|---|
| `surprise-spike` | +40% of peak for ~2 min, not in any forecast | The reactive trigger catches it; short SLO dip; IntelliOps opens a "watching" latency incident that resolves itself |
| `flash-sale` | 1.8× the trace peak (about 11 replicas' worth) for 20 lab-min, **announced** in the event calendar | Horizon pre-scales before it starts; no SLO violation |
| `ddos` | Flood from one source IP at 20× peak | Ingress rate-limits it; replicas stay at or below the cap; IntelliOps raises a "suspicious source" incident |
| `leak` | `POST /admin/leak` | IntelliOps proposes `restart-pod` |
| `bad-deploy` | Roll out `:bad` | IntelliOps proposes `rollback-deploy` |

---

## 5. Forecaster (`horizon-forecaster`)

**Signal:** the request rate *offered at the edge*, `sum(rate(nginx_ingress_controller_requests{service="loadlab-backend"}[1m]))`.
Measuring at ingress instead of the backend keeps dropped and timed-out requests in the count,
so the models learn real demand and the shadow comparison isn't biased by whichever model was in charge.

**Interface (pure, no cluster access):**

```python
class Model(Protocol):
    name: str
    def fit(self, history: pd.DataFrame) -> None: ...           # columns: ts, rps, day_type, is_holiday, event_multiplier
    def predict(self, now: pd.Timestamp, steps: int) -> Forecast: ...  # q50 and q90 per 15 s step
    def ready(self) -> bool: ...
```

**Models:**

| Model | Season | Inputs | Notes |
|---|---|---|---|
| `naive` | same time on the previous day **of the same type** (Monday copies Friday; Saturday copies last Sunday) | rps | The baseline every other model must beat |
| `hw` | **weekly** (7 × daily) | rps | A daily season would let weekend shape bleed into Monday |
| `gbm` | n/a | recent values, value one season ago, time of day, `day_type`, `is_holiday`, `event_multiplier` | `HistGradientBoostingRegressor`, one model trained on all days |

**Quantiles:** each model publishes q50 and q90.
- `naive` and `hw` get their q90 from **split-conformal calibration** on the last 7 trace days of residuals.
- `gbm` trains a second model with quantile loss at 0.9 and is then conformally calibrated the same way.
- Scaling uses **q90**. Under-forecasting a peak costs more than a spare pod.

**Cadence:** refit every 5 lab-min; predict every 15 s; the scaling value is the **max q90 over the lead window**.
The lead window is measured pod start + scrape lag, about 120 s.

**Published metrics:**

- `horizon_forecast_rps{model, quantile, horizon="lead"}`
- `horizon_forecast_rps{model="active", quantile="0.9"}` (the series KEDA reads)
- `horizon_model_ready{model}`, `horizon_last_success_timestamp`, `horizon_fit_errors_total{model}`
- `horizon_active_model{day_type, model}`

**Persistence:** every lab-day, write `history/<date>.parquet`: actual rps, every model's q50/q90 for that time,
active model, replicas. This is the trial's source of truth. It survives cluster rebuilds and allows offline re-evaluation.

---

## 6. Scaling (KEDA)

One `ScaledObject` on `loadlab-backend`, `minReplicaCount: 2`, `maxReplicaCount: 12`. KEDA takes the highest of:

1. **Forecast:** `horizon_forecast_rps{model="active",quantile="0.9"} and on() (time() - horizon_last_success_timestamp < 120)`,
   target = measured per-pod capacity at 80% utilization. A stale forecast drops out.
2. **Reactive request rate:** the ingress request rate, same target.
3. **CPU:** 70% of request. Catches capacity regressions (a slower release) that leave the request rate unchanged.

**HPA behavior:**
- **Scale up:** no stabilization window; allow +100% or +4 pods every 15 s, whichever is larger.
- **Scale down:** 300 s stabilization window, at most −1 pod per 60 s.
- **Forecast-aware scale-down:** the forecaster also publishes the max q90 over the next 10 lab-min,
  and trigger 1 uses that value whenever it's higher, so pods aren't removed just before the next peak.
- **Fallback:** if Prometheus fails 3 checks in a row, hold **6 replicas** (the trace's p99 need, section 4.1).

**Event calendar (flash sales):** a `ConfigMap` `horizon-events` lists `{name, start, end, multiplier}`.
- The forecaster multiplies its forecast by `multiplier` during the event and for the lead window before it.
- GBM sees `event_multiplier` as an input.
- The console can add events.

**Per-pod capacity:** measured by a k6 step test against `/work` (the highest rate at which one pod keeps p95 under the SLO).
It's re-measured whenever the backend image changes, and drift of more than 15% raises a console warning.

---

## 7. Rollout: shadow trial and promotion

1. **Trial (one trace month, `fast` profile):** scaling is **reactive only**. All models run and publish; none is active.
2. **Evaluation,** per `day_type`, over the whole trial, from the Parquet history:
   - MAE and sMAPE (q50)
   - **peak under-forecast rate:** the share of daily top-5% buckets where q90 < actual
   - q90 coverage (should be about 90%)
3. **Promotion rule,** applied separately to weekdays and weekends.
   - **Safety gate (every model, `naive` included):** peak under-forecast rate < 5% **and** q90 coverage between 85% and 95%.
   - **Selection:** among models that pass the gate, `naive` is the default. `hw` or `gbm` replaces it only if
     its MAE is ≥ 10% lower than `naive`'s; if both do, the lower MAE wins. A complex model has to earn its place.
   - If no model passes the gate, that day type stays reactive.
4. **Configuration:** `ConfigMap` `horizon-routing`, e.g. `{weekday: hw, weekend: naive}`. The forecaster picks the active model by `day_type`.
5. **Automatic fallback:** if a promoted model fails the same rule over a rolling 7 trace days, its day type goes back to reactive.
6. **Promotion log:** every promotion or fallback is written to `history/promotions.jsonl` with the numbers behind it, and shown in the console.

---

## 8. Surprise spikes

| Kind | Signal | Response |
|---|---|---|
| **Announced** (flash sale, launch, match) | In the event calendar | Pre-scale from the calendar (section 6). For real deployments: a waiting room on the checkout path |
| **Real users, unannounced** (viral post) | Rate above forecast q90 from **many** sources | Reactive trigger plus fast scale-up policy. Fast pod start (distroless, `/readyz` under 5 s). If at the cap: ingress sheds load with `503` + `Retry-After` instead of queueing |
| **Hostile** (DDoS) | Rate above forecast q90 from **few** sources (top-10 client IPs > 50% of requests) | **Never scale into it.** `maxReplicaCount` caps cost. Ingress `limit-rps` / `limit-connections` per IP and the server timeouts (section 3) absorb it. IntelliOps raises a "suspicious source" incident whose playbook tightens the limit or blocks the source, approved by a human |

Source concentration comes from ingress access logs (Loki in the demo profile) or a per-client-IP counter
for the top N clients exported by a small log tailer. **Open question:** which is cheaper on memory; decide during implementation.

---

## 9. IntelliOps integration (fork only, additive)

- **Overlay** `deploy/k8s/platform/values-horizon.yaml` in the fork:
  - `PROMETHEUS_URL` points at the lab's Prometheus; IntelliOps' own Prometheus and demo app are turned off.
  - Remediation namespace `loadlab`.
  - `PROMETHEUS_QUERY` drops `request_rate` (load is Horizon's job; IntelliOps watches symptoms).
  - The `scale-service` playbook is excluded. **Open question:** check how the fork loads playbooks (mounted directory vs database seed).
  - LLM explanations use template mode (no key) by default.
- **Recording rules** in the lab's Prometheus, translating LoadLab metrics into IntelliOps names with `service="loadlab-backend"`:
  `latency_p99_ms`, `latency_p50_ms`, `memory_usage_mb`, `cpu_usage`, `service_up`, plus `suspicious_source_ratio` for section 8.
- **Target resolution** already uses the `service` label as the deployment name (`services/action/targets.py`); no code change.
- **Known limitation:** an undecided approval blocks the action consumer for up to `HITL_POLL_TIMEOUT_SECONDS` (120 s).
  The demo approves each incident before the next one. A real fix is upstream work, not v1.
- **Later (v2, upstream if the teammate agrees):** forecast-aware anomaly detection. IntelliOps reads
  Horizon's forecast: load inside q90 is expected and opens no incident; load above it from few sources is "suspicious".

---

## 10. Console (`horizon-console`)

**Visual reference:** `docs/superpowers/specs/assets/console-prototype.html`, an interactive prototype with simulated data.
- **Layout:** status bar → toolbar (scaling mode, scenarios, view, speed) → stat tiles → stacked timeline
  (traffic, replicas, p95, memory on one time axis) → runs table; incidents rail on the right.
- **Look:** IntelliOps console tokens and Geist / Geist Mono.
- **Chart colors:** actual vs forecast is blue vs orange, validated for colour-blind separation in both themes.
  Single-series panels are neutral; red only above the SLO.

Additions beyond the prototype:
- A "which side won" band on the replicas pane (forecast, reactive, CPU).
- An events panel (view and add calendar entries).
- The promotion log.
- A capacity-drift warning.

**Stack:** React 18, Vite, TypeScript, Tailwind, uPlot; FastAPI backend that also serves the built frontend; one image.

**API** (the browser only talks to this):

| Endpoint | Does |
|---|---|
| `GET /api/series?from=&to=` | Prometheus range queries for every pane |
| `GET /api/state` | Active model per day type, readiness, KEDA status, events, promotion log |
| `POST /api/scenarios/{name}` | Start a k6 Job, call the backend admin port, or roll out `:bad` |
| `POST /api/events` | Add a calendar event |
| `GET /api/incidents` | IntelliOps `/situations`, `/outcomes`, `/approvals` combined |
| `POST /api/approvals/{id}/decide` | Passed on to IntelliOps governance; the token stays server-side |

- Polls every 5 s.
- **RBAC:** a `Role` in namespace `loadlab` only: `patch` on `deployments`, `create`/`delete` on `jobs`, `get`/`update` on the two ConfigMaps.
- **Local only:** no login. Reachable only through kind's port mapping on `localhost`.

---

## 11. Failure handling

Every Horizon failure degrades to plain reactive scaling, never to "no scaling".

| Failure | Result |
|---|---|
| Forecaster crashes or goes stale | Trigger 1 drops out (staleness guard); reactive and CPU triggers remain |
| Model not ready (cold start) | It publishes nothing; that day type is reactive |
| Model returns NaN, a negative or a huge value | Not published / clamped to `[0, max_rps]` |
| Refit throws | The last good model keeps serving; `horizon_fit_errors_total` increments |
| Prometheus down | KEDA fallback: 6 replicas |
| KEDA down | HPA holds the current count |
| Cluster rebuilt mid-trial | Trial resumes from Parquet history and the replayer's checkpoint |
| Flapping | Asymmetric HPA behavior (section 6) |
| Horizon and IntelliOps both acting | IntelliOps has no scaling playbook; restart and rollback never change replicas |
| DDoS | Capped at `maxReplicaCount`; ingress limits |

---

## 12. Testing and benchmark

**CI (GitHub Actions, free for public repos):**
- **Forecaster:** pytest on generated series with known answers.
  - Naive is exact on a perfectly repeating series.
  - HW and GBM beat naive on noisy, trending data.
  - Conformal q90 covers about 90% on held-out data.
  - Peak-over-window, clamping, staleness and per-day-type routing each have a test.
- **LoadLab:** Go table tests (section 3, item 10).
- **Recording rules:** `promtool test rules` with fixtures.
- **End-to-end smoke test on kind** with a 5-min day: forecasts published, `ScaledObject` Ready, replicas > 2 at the peak,
  DDoS scenario held at the cap. Pass/fail only.

**Benchmark (local, `make bench`):**
- **Setup:** 3 trace days × {reactive, naive, hw, gbm} × 3 runs × both compression profiles. Fixed seed for the scenario overlays.
- **Measures:** SLO-violation minutes (p95 > 200 ms), failed requests, replica-minutes, peak replicas.
- **Each run records** git SHA, image digests, seed, compression, kind version.
- **Output:** CSV and charts in `results/`.

**Chaos (local, `make chaos-*`):** kill the forecaster mid-ramp, stop Prometheus, publish a stale forecast, DDoS at the cap.
Each one writes what happened to latency and replicas.

---

## 13. Zero-budget operation

| Need | Free option |
|---|---|
| Kubernetes | kind on Docker Desktop (free for personal and education use) |
| Observability, scaling, load | Prometheus, Grafana, KEDA, ingress-nginx, k6, kube-state-metrics (all open source) |
| CI and images | GitHub Actions and GHCR (public repos) |
| IntelliOps LLM | Template mode; Groq free tier only for the demo recording |
| Showcase | Demo video (OBS), results page on GitHub Pages, blog on dev.to |

**Target machine:** 16 GB RAM, 8 cores (i5-12450HX), Windows 11.

- **`%USERPROFILE%\.wslconfig`:** `[wsl2] memory=8GB processors=6`.
- **Two profiles:**
  - **`trial`** (about 4–5 GB): kind, ingress-nginx, Prometheus, KEDA, kube-state-metrics, forecaster, LoadLab, replayer, console.
    No IntelliOps, no Grafana (the console replaces it).
  - **`demo`** (about 9–10 GB): `trial` + IntelliOps (template LLM) + Grafana. Run for a few hours at a time.
- **Keeping it small:** LoadLab pods are limited to 128 MiB; Prometheus keeps only the series the forecaster, KEDA, console and IntelliOps read.
- **Running the month:** about 30 h, over several nights with the laptop plugged in and sleep off. It resumes from checkpoints.
- **Optional always-on server:** Oracle Cloud Always Free (Arm, 4 OCPU, 24 GB), provisioned with **Terraform** (`infra/oci/`),
  running k3s and the `trial` profile.
  - Sign-up needs a card for identity checks.
  - Capacity in Indian regions can be unavailable.
  - Idle free servers can be reclaimed.

  It isn't on the critical path.
- **Never used:** AWS EC2/EKS, paid LLM APIs, Codespaces for long runs.

---

## 14. Phases

| Phase | Delivers | Repo |
|---|---|---|
| 1 | LoadLab hardening (section 3) + Kubernetes manifests + capacity step test | LoadLab, Horizon `deploy/` |
| 2 | kind lab: ingress-nginx, Prometheus (retention, PVC, recording rules), KEDA, kube-state-metrics; `make up` with the `trial` / `demo` profiles | Horizon |
| 3 | Traffic: NASA preprocessing, replayer with checkpoints, k6 scenarios | Horizon `traffic/` |
| 4 | Forecaster: models, conformal q90, metrics, Parquet history, routing; unit-tested with no cluster | Horizon `forecaster/` |
| 5 | KEDA ScaledObject + HPA behavior + event calendar; end-to-end smoke test in CI | Horizon `deploy/` |
| 6 | Console (from the prototype) | Horizon `console/` |
| 7 | IntelliOps overlay + recording rules + DDoS source signal | IntelliOps fork, Horizon |
| 8 | Shadow trial (one trace month), evaluation, promotion | Horizon `results/` |
| 9 | Benchmark + chaos + write-up + recording | Horizon |
| later | Oracle Always Free + Terraform; gRPC External Scaler; forecast-aware anomaly detection upstream; fixing IntelliOps' blocking approval loop | various |

---

## 15. Open questions (settle during implementation)

1. How the IntelliOps fork loads playbooks, to exclude `scale-service` (section 9).
2. Source-concentration signal: Loki or a small log tailer, whichever fits the memory budget (section 8).
3. The exact NASA-HTTP outage window, and the trace's licence and attribution wording (section 4.1).
4. ingress-nginx metric and label names on the pinned version (section 5).
