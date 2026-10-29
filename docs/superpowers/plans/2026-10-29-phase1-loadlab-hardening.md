# Phase 1: LoadLab Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the LoadLab Go backend safe to autoscale (graceful drain, timeouts, readiness, cancellable work, precise metrics, internal-only fault switches), ship it as a small non-root image, measure one pod's capacity, and run it on kind from Horizon's manifests.

**Architecture:** The backend is split from one `main.go` into small files by responsibility, in package `main`:
config, metrics, handlers, faults, server lifecycle.
- **Two listeners:** a public one on `:3000` (routed by ingress later) and an admin one on `:9091` (cluster-internal only).
- **Testing:** all state is injected (registry, clock-free tickers, random source), so every behavior is unit-testable without a cluster.
- **Horizon's role:** it gets the Kubernetes manifests and retires its own `app/`.

**Tech Stack:** Go 1.24 (module `loadlab`), `prometheus/client_golang` v1.22.0 (already a dependency), distroless static nonroot, k6 v1.7, Python 3.13 stdlib, kind v0.27.0 (Kubernetes 1.32), kubectl, kubeconform v0.6.7.

**Spec:** `docs/superpowers/specs/2026-10-29-horizon-lab-design.md`, section 3 (LoadLab hardening) and section 6 (per-pod capacity).

**Repos:** Tasks 1–6 are in the LoadLab repo (`C:/Users/Lenovo/Desktop/Projects/DevOps/Load_Testing/LoadLab`). Task 7 is in this repo.
Paths below are relative to each repo's root.

## Global Constraints

- **No new Go dependencies.** Standard library plus `github.com/prometheus/client_golang` v1.22.0 only.
- **Public port `3000`, admin port `9091`.** Admin routes are never registered on the public mux.
- **Latency SLO is 200 ms.** Histogram buckets are exactly `0.01 0.025 0.05 0.075 0.1 0.15 0.2 0.25 0.3 0.4 0.5 0.75 1 2 5`.
- **Server timeouts:** `ReadHeaderTimeout 5s`, `ReadTimeout 10s`, `WriteTimeout 15s`, `IdleTimeout 60s`, `MaxHeaderBytes 64 KiB`.
- **Shutdown drain:** 20 s maximum. Readiness turns false before `Shutdown` is called.
- **Runtime image:** `gcr.io/distroless/static:nonroot`, built with `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`. Healthy within 5 s of container start.
- **Pod resources:** `requests: cpu 250m, memory 64Mi`; `limits: cpu 1, memory 128Mi`; `terminationGracePeriodSeconds: 30`; `preStop` sleep 5 s, using the native `sleep` action because distroless has no shell.
- **Bad-deploy image:** the same Dockerfile with `FAULT_LATENCY_MS=300`, `FAULT_ERROR_RATE=0.1`, tagged `loadlab-backend:bad`.
- **Compose stays working:** the frontend still calls `/api/health`, `/api/info` and `/api/status`.
- **Existing metric names and the `route` label are unchanged**, so the provisioned Grafana dashboard keeps working.

## Review Focus

1. **Scale-down while serving.** A pod removed during load must finish its in-flight requests; zero failed requests.
   Pinned by `TestRun_DrainsInFlightRequest` (Task 4) and the scale-down check in Task 7.
2. **Admin routes reached from outside.** `POST /admin/leak` on the public port must 404.
   Pinned by `TestPublicMux_HasNoAdminRoutes` (Task 3).
3. **Nonsense configuration** (`FAULT_ERROR_RATE=1.5`, `WORK_CPU_MS=-5`, `WORK_CPU_MS=abc`): clamp or fall back to defaults with a log line, never crash or run unbounded.
   Pinned by `TestLoadConfig_ClampsAndFallsBack` (Task 1).
4. **Leak rate abuse** (`mb_per_min=100000`): clamp to 200 MB/min; a missing or invalid value is a 400.
   Pinned by `TestAdminLeak_ClampsAndValidates` (Task 3).
5. **Slowloris client** (sends headers byte by byte): the connection is closed after `ReadHeaderTimeout`.
   Pinned by `TestPublicServer_ClosesSlowHeaderClient` (Task 4).

## Prerequisites

- **LoadLab working tree:** it has your uncommitted `README.md` change and an untracked `council-report-2026-10-05-2340.html`.
  Commit or stash them first, then create a branch: `git switch -c feat/horizon-hardening`.
- **This repo's working tree:** commit or discard your staged `.github/workflows/ci.yml` (end-of-file newline) and unstaged `.gitignore` edits before Task 7, which rewrites `ci.yml`.
- **Docker Desktop running.**
- **kind installed:** `go install sigs.k8s.io/kind@v0.27.0`. Verify: `kind version` prints `kind v0.27.0`.

---

### Task 1: Config and metrics

**Files:**
- Create: `backend/config.go`, `backend/metrics.go`
- Test: `backend/config_test.go`, `backend/metrics_test.go`
- Modify: `backend/main.go` (remove the old metric vars and `instrumentHandler`; temporary wiring until Task 4)

**Interfaces:**
- Produces:
  - `type Config struct { WorkCPU, WorkWait, WorkTimeout time.Duration; FaultLatency time.Duration; FaultErrorRate float64; Version string; StatusTargets map[string]string; Drain time.Duration }`
  - `func LoadConfig(getenv func(string) string, logf func(string, ...any)) Config`
  - `type Metrics struct { /* requests *CounterVec, duration *HistogramVec, inFlight Gauge */ }`
  - `func NewMetrics(reg prometheus.Registerer, version string) *Metrics`
  - `func (m *Metrics) Instrument(route string, h http.HandlerFunc) http.HandlerFunc`

- [ ] **Step 1: Write the failing tests**

```go
func TestLoadConfig_Defaults(t *testing.T) {
    c := LoadConfig(func(string) string { return "" }, t.Logf)
    assertEq(t, c.WorkCPU, 20*time.Millisecond)
    assertEq(t, c.WorkWait, 30*time.Millisecond)
    assertEq(t, c.WorkTimeout, 5*time.Second)
    assertEq(t, c.FaultLatency, time.Duration(0))
    assertEq(t, c.FaultErrorRate, 0.0)
    assertEq(t, c.Version, "dev")
    assertEq(t, c.Drain, 20*time.Second)
    assertEq(t, c.StatusTargets["prometheus"], "http://prometheus:9090/-/healthy")
    assertEq(t, c.StatusTargets["grafana"], "http://grafana:3000/api/health")
}

func TestLoadConfig_ClampsAndFallsBack(t *testing.T) {
    env := map[string]string{"WORK_CPU_MS": "-5", "WORK_DURATION_MS": "abc", "FAULT_ERROR_RATE": "1.5", "FAULT_LATENCY_MS": "999999"}
    var logged []string
    c := LoadConfig(func(k string) string { return env[k] }, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
    assertEq(t, c.WorkCPU, 20*time.Millisecond)       // negative → default
    assertEq(t, c.WorkWait, 30*time.Millisecond)      // unparsable → default
    assertEq(t, c.FaultErrorRate, 1.0)                // clamped to [0,1]
    assertEq(t, c.FaultLatency, 10*time.Second)       // clamped to 10 s
    if len(logged) != 4 { t.Fatalf("want 4 warnings, got %v", logged) }
}

func TestLoadConfig_StatusTargetsFromEnv(t *testing.T) {
    c := LoadConfig(func(k string) string {
        if k == "STATUS_TARGETS" { return "prometheus=http://p:9090/-/healthy,keda=http://k:8080/healthz" }; return "" }, t.Logf)
    assertEq(t, len(c.StatusTargets), 2)
    assertEq(t, c.StatusTargets["keda"], "http://k:8080/healthz")
}

func TestMetrics_BucketsMatchSLO(t *testing.T) {
    reg := prometheus.NewRegistry()
    m := NewMetrics(reg, "v1")
    m.Instrument("/work", func(w http.ResponseWriter, r *http.Request) {})(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
    // gather http_request_duration_seconds; bucket upper bounds must equal exactly:
    want := []float64{0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.75, 1, 2, 5}
    assertBuckets(t, reg, "http_request_duration_seconds", want)
}

func TestMetrics_InstrumentCountsStatusAndInFlight(t *testing.T) {
    reg := prometheus.NewRegistry()
    m := NewMetrics(reg, "v1")
    seen := -1.0
    h := m.Instrument("/work", func(w http.ResponseWriter, r *http.Request) {
        seen = testutil.ToFloat64(m.inFlight); w.WriteHeader(http.StatusTeapot) })
    h(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
    assertEq(t, seen, 1.0)                                         // gauge raised during the request
    assertEq(t, testutil.ToFloat64(m.inFlight), 0.0)               // and lowered after
    assertEq(t, testutil.ToFloat64(m.requests.WithLabelValues("/work", "418")), 1.0)
    assertEq(t, gaugeValue(t, reg, "build_info", map[string]string{"version": "v1"}), 1.0)
}
```

`assertEq`, `assertBuckets` and `gaugeValue` are small helpers in `backend/helpers_test.go`.
`testutil` is `github.com/prometheus/client_golang/prometheus/testutil`, which is part of the existing dependency.

- [ ] **Step 2: Run them to see them fail**

Run: `cd backend && go test ./... -run 'TestLoadConfig|TestMetrics' -v`
Expected: FAIL, `undefined: LoadConfig` / `undefined: NewMetrics`

- [ ] **Step 3: Implement `LoadConfig` in `backend/config.go`**

Environment variables:

| Variable | Default | Valid range |
|---|---|---|
| `WORK_CPU_MS` | 20 | 0–1000 |
| `WORK_DURATION_MS` | 30 | 0–10000 |
| `WORK_TIMEOUT_MS` | 5000 | 100–30000 |
| `FAULT_LATENCY_MS` | 0 | 0–10000, clamp |
| `FAULT_ERROR_RATE` | 0 | 0–1, clamp |
| `APP_VERSION` | `dev` | n/a |
| `SHUTDOWN_DRAIN_S` | 20 | 1–25 |
| `STATUS_TARGETS` | `name=url,...` | n/a |

Behavior:
- Negative or unparsable values fall back to the default; values above the range are clamped. Each one logs exactly one warning.
- Without `STATUS_TARGETS`, the default is the two Compose URLs.

- [ ] **Step 4: Implement `NewMetrics` and `Instrument` in `backend/metrics.go`**

- Register on `reg` via `promauto.With(reg)`:
  - `http_requests_total{route,status}`
  - `http_request_duration_seconds{route}` with the constraint buckets
  - `http_requests_in_flight`
  - `build_info{version}`, set to 1
- `Instrument` raises and lowers the gauge around the handler and reuses the existing `statusRecorder` (moved here from `main.go`).

- [ ] **Step 5: Run the tests**

Run: `cd backend && go test ./... -run 'TestLoadConfig|TestMetrics' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add backend/config.go backend/config_test.go backend/metrics.go backend/metrics_test.go backend/helpers_test.go backend/main.go
git commit -m "feat(backend): validated config and SLO-grade metrics with in-flight gauge"
```

---

### Task 2: Cancellable handlers, readiness and `/work`

**Files:**
- Create: `backend/handlers.go`
- Test: `backend/handlers_test.go`
- Modify: `backend/main.go` (handlers move out)

**Interfaces:**
- Consumes: `Config` (Task 1).
- Produces:
  - `type Health struct { /* ready, draining atomic.Bool */ }` with `SetReady(bool)`, `SetDraining()`, `Live(w, r)`, `Ready(w, r)`
  - `func Work(cfg Config) http.HandlerFunc`
  - `func Slow(delay time.Duration) http.HandlerFunc` (production value 3 s)
  - `func CPU(limit int) http.HandlerFunc` (production value 1_000_000)
  - `func Memory(w, r)`
  - `func Info(cfg Config, start time.Time) http.HandlerFunc`
  - `func Status(cfg Config, client *http.Client) http.HandlerFunc`
  - `func burnCPU(ctx context.Context, d time.Duration) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestWork_SpendsCPUThenWaits(t *testing.T) {
    cfg := Config{WorkCPU: 30 * time.Millisecond, WorkWait: 20 * time.Millisecond, WorkTimeout: time.Second}
    rec := httptest.NewRecorder(); t0 := time.Now()
    Work(cfg)(rec, httptest.NewRequest("GET", "/work", nil))
    assertEq(t, rec.Code, 200)
    if d := time.Since(t0); d < 50*time.Millisecond || d > 250*time.Millisecond { t.Fatalf("took %v", d) }
}

func TestWork_TimeoutReturns504(t *testing.T) {
    cfg := Config{WorkCPU: 0, WorkWait: 500 * time.Millisecond, WorkTimeout: 50 * time.Millisecond}
    rec := httptest.NewRecorder()
    Work(cfg)(rec, httptest.NewRequest("GET", "/work", nil))
    assertEq(t, rec.Code, http.StatusGatewayTimeout)
}

func TestBurnCPU_StopsOnCancel(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond); defer cancel()
    t0 := time.Now()
    err := burnCPU(ctx, 5*time.Second)
    if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > 100*time.Millisecond { t.Fatalf("err=%v after %v", err, time.Since(t0)) }
}

func TestSlowAndCPU_StopWhenClientGoes(t *testing.T) {
    for name, h := range map[string]http.HandlerFunc{"slow": Slow(3 * time.Second), "cpu": CPU(1 << 40)} {
        ctx, cancel := context.WithCancel(context.Background())
        go func() { time.Sleep(20 * time.Millisecond); cancel() }()
        t0 := time.Now()
        h(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
        if time.Since(t0) > 200*time.Millisecond { t.Fatalf("%s kept running %v after cancel", name, time.Since(t0)) }
    }
}

func TestReady_FollowsLifecycle(t *testing.T) {
    var h Health
    assertEq(t, code(h.Ready), 503)          // not ready before init
    h.SetReady(true);  assertEq(t, code(h.Ready), 200)
    h.SetDraining();   assertEq(t, code(h.Ready), 503)
    assertEq(t, code(h.Live), 200)          // still alive while draining
}

func TestStatus_UsesConfiguredTargets(t *testing.T) {
    up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
    defer up.Close()
    cfg := Config{StatusTargets: map[string]string{"prometheus": up.URL, "ghost": "http://127.0.0.1:1"}}
    rec := httptest.NewRecorder()
    Status(cfg, &http.Client{Timeout: time.Second})(rec, httptest.NewRequest("GET", "/status", nil))
    var got map[string]struct{ Status string }
    _ = json.NewDecoder(rec.Body).Decode(&got)
    assertEq(t, got["prometheus"].Status, "online"); assertEq(t, got["ghost"].Status, "offline")
}
```

`code(h)` runs a handler against a recorder and returns the status code (in `helpers_test.go`).

- [ ] **Step 2: Run them to see them fail**

Run: `cd backend && go test ./... -run 'TestWork|TestBurnCPU|TestSlowAndCPU|TestReady|TestStatus' -v`
Expected: FAIL, undefined symbols

- [ ] **Step 3: Implement the handlers in `backend/handlers.go`**

- **`burnCPU`:** spins in a tight loop, checking `ctx.Err()` every 1 ms of wall time. It returns `nil` when `d` has elapsed and `ctx.Err()` when cancelled.
- **`Work`:** uses a `context.WithTimeout(r.Context(), cfg.WorkTimeout)`. It runs `burnCPU(ctx, cfg.WorkCPU)`, then waits `cfg.WorkWait` on a timer or `ctx.Done()`.
  - Deadline exceeded → `504`.
  - Client gone → return without writing.
  - Success → `200 {"status":"ok","cpu_ms":…,"wait_ms":…}`.
- **`Slow`:** selects on a timer vs `r.Context().Done()`.
- **`CPU`:** checks `r.Context().Err()` every 10 000 numbers.
- **`Memory`:** unchanged.
- **`Info`:** adds `version`.
- **`Status`:** probes each `cfg.StatusTargets` entry concurrently, using the existing online/offline rule.
- **`Health.Ready`:** 200 only when ready and not draining; otherwise `503 {"status":"not ready"|"draining"}`. `Live` is always 200.

- [ ] **Step 4: Run the tests**

Run: `cd backend && go test ./... -run 'TestWork|TestBurnCPU|TestSlowAndCPU|TestReady|TestStatus' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add backend/handlers.go backend/handlers_test.go backend/helpers_test.go backend/main.go
git commit -m "feat(backend): cancellable handlers, /work with real CPU cost, live/ready probes"
```

---

### Task 3: Faults and the admin listener

**Files:**
- Create: `backend/faults.go`, `backend/routes.go`
- Test: `backend/faults_test.go`, `backend/routes_test.go`

**Interfaces:**
- Consumes: `Config`, `Metrics.Instrument`, the handlers and `Health` (Tasks 1–2).
- Produces:
  - `type Leaker struct { ... }`
    - `func NewLeaker(tick <-chan time.Time) *Leaker` (production: `time.Tick(time.Second)`)
    - `Start(mbPerMin int)`, `Stop()`, `HeldMB() int`
  - `func FaultMiddleware(cfg Config, rnd func() float64, next http.Handler) http.Handler`
  - `func PublicMux(cfg Config, m *Metrics, h *Health, start time.Time) http.Handler`
  - `func AdminMux(l *Leaker) http.Handler`

- [ ] **Step 1: Write the failing tests**

```go
func TestLeaker_GrowsPerTickAndStopReleases(t *testing.T) {
    tick := make(chan time.Time)
    l := NewLeaker(tick)
    l.Start(60)                                  // 60 MB/min = 1 MB per 1 s tick
    for i := 0; i < 3; i++ { tick <- time.Now() }
    waitFor(t, func() bool { return l.HeldMB() == 3 })
    l.Stop()
    assertEq(t, l.HeldMB(), 0)
}

func TestAdminLeak_ClampsAndValidates(t *testing.T) {
    l := NewLeaker(make(chan time.Time)); mux := AdminMux(l)
    assertEq(t, post(mux, "/admin/leak?mb_per_min=100000").Code, 202)
    assertEq(t, l.rate(), 200)                   // clamped
    assertEq(t, post(mux, "/admin/leak").Code, 400)
    assertEq(t, post(mux, "/admin/leak?mb_per_min=-3").Code, 400)
    assertEq(t, get(mux, "/admin/leak").Code, 405)
    assertEq(t, post(mux, "/admin/leak/stop").Code, 202)
}

func TestFaultMiddleware_OffByDefault(t *testing.T) {
    h := FaultMiddleware(Config{}, func() float64 { return 0 }, ok200())
    t0 := time.Now(); rec := serve(h, "/work")
    assertEq(t, rec.Code, 200)
    if time.Since(t0) > 20*time.Millisecond { t.Fatal("added latency with faults off") }
}

func TestFaultMiddleware_AddsLatencyAndErrors(t *testing.T) {
    cfg := Config{FaultLatency: 50 * time.Millisecond, FaultErrorRate: 0.1}
    t0 := time.Now()
    assertEq(t, serve(FaultMiddleware(cfg, func() float64 { return 0.05 }, ok200()), "/work").Code, 500)  // 0.05 < 0.1 → error
    if time.Since(t0) < 50*time.Millisecond { t.Fatal("latency not applied") }
    assertEq(t, serve(FaultMiddleware(cfg, func() float64 { return 0.5 }, ok200()), "/work").Code, 200)
}

func TestFaultMiddleware_SparesProbesAndMetrics(t *testing.T) {
    cfg := Config{FaultErrorRate: 1}
    for _, p := range []string{"/livez", "/readyz", "/metrics"} {
        assertEq(t, serve(FaultMiddleware(cfg, func() float64 { return 0 }, ok200()), p).Code, 200)
    }
}

func TestPublicMux_HasNoAdminRoutes(t *testing.T) {
    mux := PublicMux(Config{}, NewMetrics(prometheus.NewRegistry(), "t"), &Health{}, time.Now())
    assertEq(t, post(mux, "/admin/leak?mb_per_min=10").Code, 404)
    for _, p := range []string{"/work", "/slow", "/cpu", "/memory", "/info", "/status", "/health", "/livez", "/readyz", "/metrics"} {
        if serve(mux, p).Code == 404 { t.Errorf("%s missing", p) }
    }
}
```

Note: `/slow` and `/cpu` take seconds to answer. For the route-existence loop, use a request whose context is already cancelled; the test only checks "not 404".

- [ ] **Step 2: Run them to see them fail**

Run: `cd backend && go test ./... -run 'TestLeaker|TestAdmin|TestFault|TestPublicMux' -v`
Expected: FAIL, undefined symbols

- [ ] **Step 3: Implement `backend/faults.go` and `backend/routes.go`**

- **`Leaker`:** holds `[][]byte` chunks behind a mutex. Each tick it appends `rate/60` MB and touches every page so the memory is really resident. `Stop` drops the slice and calls `debug.FreeOSMemory()`.
- **`FaultMiddleware`:** skips `/livez`, `/readyz`, `/health`, `/metrics`. It sleeps `FaultLatency` (cancellable), then returns 500 when `rnd() < FaultErrorRate`.
- **`PublicMux`:** every route goes through `m.Instrument(route, …)`. `/health` is an alias of `Live` for the Compose frontend. The whole mux is wrapped in `FaultMiddleware(cfg, rand.Float64, …)`.
- **`AdminMux`:**
  - `POST /admin/leak?mb_per_min=N`: N must be an integer from 1 upward, clamped to 200; otherwise 400.
  - `POST /admin/leak/stop`.
  - Anything else is 404 or 405.

- [ ] **Step 4: Run the tests**

Run: `cd backend && go test ./... -run 'TestLeaker|TestAdmin|TestFault|TestPublicMux' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add backend/faults.go backend/faults_test.go backend/routes.go backend/routes_test.go backend/helpers_test.go
git commit -m "feat(backend): internal-only admin listener with leak switch, env-driven fault injection"
```

---

### Task 4: Server lifecycle (timeouts, graceful drain)

**Files:**
- Create: `backend/server.go`
- Test: `backend/server_test.go`
- Modify: `backend/main.go` (reduced to: load config, build muxes, `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`, `os.Exit` on a non-nil `Run` error)

**Interfaces:**
- Consumes: `Config`, `PublicMux`, `AdminMux`, `Health` (Tasks 1–3).
- Produces:
  - `func NewPublicServer(addr string, h http.Handler) *http.Server`
  - `func Run(ctx context.Context, cfg Config, health *Health, public, admin *http.Server, logf func(string, ...any)) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestPublicServer_Timeouts(t *testing.T) {
    s := NewPublicServer(":0", http.NewServeMux())
    assertEq(t, s.ReadHeaderTimeout, 5*time.Second); assertEq(t, s.ReadTimeout, 10*time.Second)
    assertEq(t, s.WriteTimeout, 15*time.Second);     assertEq(t, s.IdleTimeout, 60*time.Second)
    assertEq(t, s.MaxHeaderBytes, 64<<10)
}

func TestPublicServer_ClosesSlowHeaderClient(t *testing.T) {
    s := NewPublicServer("127.0.0.1:0", http.NewServeMux())
    s.ReadHeaderTimeout = 200 * time.Millisecond             // shortened for the test
    ln := listen(t); go s.Serve(ln); defer s.Close()
    c, _ := net.Dial("tcp", ln.Addr().String()); defer c.Close()
    _, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // never finishes the headers
    _ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
    t0 := time.Now(); _, err := c.Read(make([]byte, 1))
    if err == nil || time.Since(t0) > time.Second { t.Fatalf("slow client not cut off: err=%v after %v", err, time.Since(t0)) }
}

func TestRun_DrainsInFlightRequest(t *testing.T) {
    cfg := Config{WorkWait: 300 * time.Millisecond, WorkTimeout: 2 * time.Second, Drain: 2 * time.Second}
    var h Health
    pub := NewPublicServer("127.0.0.1:0", PublicMux(cfg, NewMetrics(prometheus.NewRegistry(), "t"), &h, time.Now()))
    adm := &http.Server{Addr: "127.0.0.1:0", Handler: AdminMux(NewLeaker(make(chan time.Time)))}
    ctx, stop := context.WithCancel(context.Background())
    done := make(chan error, 1); go func() { done <- Run(ctx, cfg, &h, pub, adm, t.Logf) }()
    base := waitListening(t, pub)                         // helper: polls until Addr resolves and /readyz is 200
    resp := make(chan int, 1); go func() { r, err := http.Get(base + "/work"); if err != nil { resp <- -1; return }; resp <- r.StatusCode }()
    time.Sleep(50 * time.Millisecond); stop()             // SIGTERM equivalent, mid-request
    assertEq(t, <-resp, 200)                              // in-flight request finished
    assertEq(t, <-done, nil)
    assertEq(t, code(h.Ready), 503)                       // readiness went false
}
```

`listen` and `waitListening` live in `helpers_test.go`.
`Run` must expose the bound address when `Addr` ends in `:0`: it binds with `net.Listen` and stores `ln.Addr()` back into `server.Addr`.

- [ ] **Step 2: Run them to see them fail**

Run: `cd backend && go test ./... -run 'TestPublicServer|TestRun' -v`
Expected: FAIL, undefined symbols

- [ ] **Step 3: Implement `backend/server.go`**

**`Run`:**
1. Bind both listeners.
2. `health.SetReady(true)`.
3. Serve both servers in goroutines.
4. On `<-ctx.Done()`: `health.SetDraining()`, then `Shutdown` both servers with a `context.WithTimeout(context.Background(), cfg.Drain)`.
5. Return the first non-`ErrServerClosed` error, or `nil`.

Log `"draining"` and `"stopped"` lines through `logf`.

- [ ] **Step 4: Run the whole suite with the race detector**

Run: `cd backend && go vet ./... && go test -race -count=1 ./...`
Expected: PASS, no race reports.
On Windows `-race` needs CGO and gcc; if that's missing, run `go test -count=1 ./...` here and rely on CI (Linux) for `-race`.

- [ ] **Step 5: Commit**

```bash
git add backend/server.go backend/server_test.go backend/helpers_test.go backend/main.go
git commit -m "feat(backend): server timeouts and graceful drain on SIGTERM"
```

---

### Task 5: Image, Compose cleanup and CI

**Files:**
- Modify: `backend/Dockerfile`, `docker-compose.yml`, `README.md` (new endpoints, admin port, `:bad` image)
- Create: `.github/workflows/backend.yml`

**Interfaces:**
- Produces: images `loadlab-backend:local` and `loadlab-backend:bad`, used by Tasks 6 and 7.

- [ ] **Step 1: Rewrite `backend/Dockerfile`**

- Builder: `golang:1.24-alpine`, `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.buildVersion=${VERSION}" -o /server .`
  - Add `var buildVersion = "dev"` in `main.go`. `APP_VERSION` overrides it.
- Runtime: `gcr.io/distroless/static:nonroot`, `USER nonroot:nonroot`, `EXPOSE 3000 9091`.
- Build args `VERSION`, `FAULT_LATENCY_MS=0`, `FAULT_ERROR_RATE=0`, carried into `ENV`.

- [ ] **Step 2: Build both images**

```bash
docker build -t loadlab-backend:local --build-arg VERSION=local backend
docker build -t loadlab-backend:bad --build-arg VERSION=bad --build-arg FAULT_LATENCY_MS=300 --build-arg FAULT_ERROR_RATE=0.1 backend
docker image ls loadlab-backend
```

Expected: both build; size under 25 MB.

- [ ] **Step 3: Check the start-up budget and that the admin port is separate**

```bash
docker run -d --name ll -p 3000:3000 loadlab-backend:local
for i in $(seq 1 50); do curl -sf localhost:3000/readyz && break; sleep 0.1; done; echo "ready after ~$((i*100)) ms"
curl -s -o /dev/null -w "%{http_code}\n" -X POST "localhost:3000/admin/leak?mb_per_min=10"
docker rm -f ll
```

Expected: ready in under 5000 ms; the admin call on the public port prints `404`.

- [ ] **Step 4: Clean up `docker-compose.yml`**

- Remove `GF_SERVER_DOMAIN` and the hard-coded `43.204.103.85` from `GF_SERVER_ROOT_URL`. Use `%(protocol)s://%(domain)s:%(http_port)s/grafana/`.
- Do not publish `9091` in Compose.
- `docker compose up -d --build`, open `http://localhost/` and confirm the dashboard's health, info and status cards load. Then `docker compose down`.

- [ ] **Step 5: Add CI `.github/workflows/backend.yml`**

- Ubuntu job with `actions/setup-go@v5` (`go-version-file: backend/go.mod`).
- `go vet ./...`, `go test -race -count=1 ./...`, then `docker build` of both tags (no push).

- [ ] **Step 6: Commit and check CI**

```bash
git add backend/Dockerfile backend/main.go docker-compose.yml README.md .github/workflows/backend.yml
git commit -m "build(backend): distroless nonroot image, :bad fault image, compose cleanup, CI"
```

After pushing the branch, check with `gh run list --branch feat/horizon-hardening -L 1`. Expected: `completed success`.

---

### Task 6: Per-pod capacity step test

**Files:**
- Create: `k6/capacity-test.js`, `scripts/capacity.py`, `scripts/test_capacity.py`
- Output: `results/capacity.json` (committed)

**Interfaces:**
- Consumes: image `loadlab-backend:local` (Task 5).
- Produces:
  - `results/capacity.json`, shaped as `{"rps_per_pod": int, "slo_ms": 200, "image": str, "levels": [{"rps": int, "p95_ms": float, "failed_rate": float}], "measured_at": iso8601}`
  - Spec section 6 uses `rps_per_pod` (at 80% utilization: target = `floor(0.8 * rps_per_pod)`).

- [ ] **Step 1: Write the failing test**

```python
class CapacityFromLevels(unittest.TestCase):
    def test_last_level_under_slo(self):
        levels = [{"rps": 10, "p95_ms": 60, "failed_rate": 0}, {"rps": 20, "p95_ms": 120, "failed_rate": 0},
                  {"rps": 30, "p95_ms": 240, "failed_rate": 0}]
        self.assertEqual(capacity_from_levels(levels, slo_ms=200), 20)

    def test_failures_disqualify_a_level(self):
        levels = [{"rps": 10, "p95_ms": 60, "failed_rate": 0}, {"rps": 20, "p95_ms": 90, "failed_rate": 0.02}]
        self.assertEqual(capacity_from_levels(levels, slo_ms=200), 10)

    def test_first_level_already_over_slo_is_an_error(self):
        with self.assertRaises(ValueError):
            capacity_from_levels([{"rps": 10, "p95_ms": 500, "failed_rate": 0}], slo_ms=200)
```

- [ ] **Step 2: Run it to see it fail**

Run: `python -m unittest scripts/test_capacity.py -v`
Expected: FAIL, `ImportError` / `NameError: capacity_from_levels`

- [ ] **Step 3: Implement**

- **`k6/capacity-test.js`:**
  - `constant-arrival-rate` at `__ENV.RATE` rps for `__ENV.DURATION` (default `60s`) against `${BASE_URL}/work`, with `preAllocatedVUs: 200`.
  - `handleSummary` writes `{p95_ms, failed_rate}` to `__ENV.OUT`.
- **`scripts/capacity.py`** (stdlib only):
  - Starts `docker run -d --cpus 1 -m 128m -p 3000:3000 loadlab-backend:local`.
  - Waits for `/readyz`.
  - Runs k6 at 10, 20, 30… rps (step 10), stopping at the first level over SLO or `failed_rate > 0.01`.
  - `capacity_from_levels(levels, slo_ms)` returns the highest passing rps before the first failing level, and raises `ValueError` if none pass.
  - Writes `results/capacity.json` and always removes the container.

- [ ] **Step 4: Run the unit test, then the real measurement**

Run: `python -m unittest scripts/test_capacity.py -v` → PASS
Run: `python scripts/capacity.py`

Expected: prints `rps_per_pod=<N>` with N roughly 25–45 at the default `WORK_CPU_MS=20`, and writes `results/capacity.json`.
If N falls outside that range, record the value as measured anyway; the spec reads it from the file.

- [ ] **Step 5: Commit**

```bash
git add k6/capacity-test.js scripts/capacity.py scripts/test_capacity.py results/capacity.json
git commit -m "test(capacity): per-pod capacity step test at 1 CPU / 128 MiB"
```

---

### Task 7: Kubernetes manifests in Horizon; retire `app/`

Repo: Horizon-Autoscaler. Branch: `feat/phase1-loadlab-manifests`, from `docs/horizon-lab-spec`. Complete the prerequisite about your `ci.yml` / `.gitignore` edits first.

**Files:**
- Create: `deploy/loadlab/namespace.yaml`, `deploy/loadlab/deployment.yaml`, `deploy/loadlab/service.yaml`, `deploy/loadlab/service-admin.yaml`, `deploy/kind/cluster.yaml`
- Modify: `Makefile` (replace the Go targets), `.github/workflows/ci.yml` (replace the Go job with manifest validation), `README.md` (Roadmap section points to the spec's phase table)
- Delete: `app/` (`git rm -r app`)

**Interfaces:**
- Consumes:
  - images `loadlab-backend:local` and `:bad` (Task 5)
  - `results/capacity.json` from the LoadLab repo (Task 6): copy `rps_per_pod` into `deploy/loadlab/deployment.yaml` as the annotation `horizon.dev/rps-per-pod`
- Produces (Phase 2 and later rely on these):
  - namespace `loadlab`
  - Deployment and Service `loadlab-backend` (port `http`: 3000)
  - Service `loadlab-backend-admin` (port `admin`: 9091)
  - pod label `app.kubernetes.io/name: loadlab-backend`

- [ ] **Step 1: Write the manifests**

- **`deployment.yaml`:**
  - `replicas: 2`, image `loadlab-backend:local`, `imagePullPolicy: IfNotPresent`.
  - The resources, `terminationGracePeriodSeconds` and `lifecycle.preStop.sleep.seconds: 5` from Global Constraints.
  - `readinessProbe httpGet /readyz` (period 2 s, failureThreshold 1); `livenessProbe httpGet /livez` (period 10 s, failureThreshold 3).
  - `securityContext: runAsNonRoot: true, allowPrivilegeEscalation: false, readOnlyRootFilesystem: true`.
  - Env `APP_VERSION` from the image tag.
- **`service.yaml`:** exposes `http` only.
- **`service-admin.yaml`:** ClusterIP, exposes `admin` only. It is never referenced by any Ingress.
- **`deploy/kind/cluster.yaml`:** a single node, named `horizon-lab`.

- [ ] **Step 2: Validate the manifests offline**

Run: `docker run --rm -v "$PWD/deploy:/d" ghcr.io/yannh/kubeconform:v0.6.7 -strict -summary /d/loadlab`
Expected: `Summary: 4 resources found ... Valid: 4, Invalid: 0, Errors: 0`

- [ ] **Step 3: Bring it up on kind**

```bash
kind create cluster --config deploy/kind/cluster.yaml
kind load docker-image loadlab-backend:local --name horizon-lab
kubectl apply -f deploy/loadlab/
kubectl -n loadlab rollout status deploy/loadlab-backend --timeout=120s
```

Expected: `successfully rolled out`, with 2/2 pods Ready.

- [ ] **Step 4: Scale down under load with zero failed requests (Review Focus 1)**

```bash
kubectl -n loadlab port-forward svc/loadlab-backend 3000:3000 &
k6 run -e RATE=20 -e DURATION=60s -e BASE_URL=http://localhost:3000 -e OUT=/tmp/drain.json ../DevOps/Load_Testing/LoadLab/k6/capacity-test.js &
sleep 20; kubectl -n loadlab scale deploy/loadlab-backend --replicas=1; wait %2
python -c "import json; d=json.load(open('/tmp/drain.json')); print(d); assert d['failed_rate']==0"
```

Expected: `failed_rate` is 0. Kill the port-forward afterwards.

**Caveat:** `kubectl port-forward` pins one pod. If the forwarded pod is the one removed, the forward itself breaks, which says nothing about the drain. Run it twice, or instead `kubectl -n loadlab run k6 --image=grafana/k6:1.7.1` inside the cluster against the Service. The in-cluster run is the authoritative check.

- [ ] **Step 5: Replace `app/`, the Makefile and CI**

- `git rm -r app`.
- **Makefile targets:**
  - `cluster-up`: create the cluster and load the image
  - `deploy`: apply the manifests and wait for rollout
  - `validate`: kubeconform
  - `cluster-down`: delete the cluster
- **`ci.yml` job "Validate manifests":** checkout, then run kubeconform on `deploy/` (`-strict`), using the pinned `v0.6.7` release binary.
- **README Roadmap:** replace the list with one line linking `docs/superpowers/specs/2026-10-29-horizon-lab-design.md#14-phases`, and mark Phase 1 done.

- [ ] **Step 6: Run validation and tear down**

Run: `make validate && make cluster-down`
Expected: kubeconform valid; cluster deleted.

- [ ] **Step 7: Commit**

```bash
git add deploy Makefile .github/workflows/ci.yml README.md
git commit -m "feat(deploy): LoadLab manifests for kind; retire horizon-app in favour of LoadLab"
```

---

## Not in this plan (later phase plans)

- ingress-nginx, Prometheus, KEDA, kube-state-metrics, and `make up` with the profiles (Phase 2)
- the NASA-HTTP trace, the replayer and the scenarios (Phase 3)
- the forecaster (Phase 4); scaling (Phase 5); the console (Phase 6); IntelliOps (Phase 7)
- the trial (Phase 8); the benchmark (Phase 9)
