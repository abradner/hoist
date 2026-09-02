# Rails Prometheus metrics

> Recipe for instrumenting a Rails service with a Prometheus `/metrics` endpoint from day one,
> and for wiring the scrape on the cluster side. Proven across four production Rails 8.1 / Puma
> apps (spritz, kaff, counta, crm) that were retrofitted in one pass — this doc exists so the
> next service never needs the retrofit. It is Rails-specific; skip it for anything else.
>
> If this repo is not a Rails service, delete this file at init.

## Why not just structured request logs

Request logs (Thruster's JSON, or lograge-style) can approximate request rate / error rate /
latency with log-store stats queries, and dashboards can be built that way — ours were. But it
is a workaround: percentiles are approximated instead of coming from histograms, every panel
becomes a heavy log query, and the log store does numeric aggregation the metrics store is built
for. Logs also sit *in front of* Rails (Thruster is a Go proxy), so they can never see the route
template, the controller, or DB/view time. Instrument the app; let logs be logs.

## The gem choice: yabeda

```ruby
# Prometheus metrics: request histograms (yabeda-rails), Puma pool gauges
# (yabeda-puma-plugin), Ruby GC/heap stats (yabeda-gc — the signal that tells
# heap growth from native growth when a container's RSS climbs), and a
# /metrics exporter on a separate in-process port (yabeda-prometheus).
# See config/puma.rb.
gem "yabeda-gc"
gem "yabeda-prometheus"
gem "yabeda-puma-plugin"
gem "yabeda-rails"
```

Chosen over `prometheus_exporter` (Discourse's) because:

- **Cardinality safety by construction.** `yabeda-rails` subscribes to
  `process_action.action_controller` and labels by `controller`, `action`, `status`, `format`,
  `method`. There is *no path label at all*, so the classic footgun — raw `/users/123` paths
  exploding the series count — cannot happen. `controller#action` is exactly the collapse a
  route-template label (`/users/:id`) would give you. Verified: hitting one show route with
  three different ids produces one series.
- **Puma pool visibility is a first-class plugin**, not an add-on. On RAM-constrained hardware,
  `puma_busy_threads` vs `puma_max_threads` is the saturation signal you want before the OOM.
- **No collector process.** `prometheus_exporter`'s model (app pushes to a separate collector)
  is one more container per pod. Yabeda's exporter runs a one-thread Puma server inside the
  existing process.
- Single-mode Puma (no `workers`) means the default in-memory registry is correct. If you turn
  on clustered Puma, revisit — you'd need `yabeda-prometheus-mmap` or per-worker aggregation.
- **`yabeda-gc` is the memory-leak triage kit.** `container_memory_working_set_bytes` is a
  cgroup total: it cannot distinguish Ruby heap from native allocation, or one process from
  another in the pod. A production leak hunt without in-process metrics reduces to guessing.
  `gc_heap_live_slots` / `gc_old_objects` climbing with RSS = Ruby heap leak;
  flat heap slots under climbing RSS = native memory or fragmentation. The gem self-registers
  on require and reads `GC.stat` per scrape — zero config, ~30 series.

Coverage boundaries to know about (don't discover them during an incident):

- **Replicas are free**: `VMServiceScrape` discovers every ready endpoint behind the Service,
  so HPA scale-out just adds per-pod series (`instance` label distinguishes them).
- **Non-Puma Rails processes are NOT covered.** yabeda-rails installs only when it detects a
  server, and the exporter is a Puma plugin — a Solid Queue / jobs Deployment running
  `bin/jobs` gets no metrics from this recipe. If queue observability is needed, that is a
  separate piece (`yabeda-sidekiq`-style plugin or a small exporter in the supervisor), not a
  tweak to this one — scope it separately.

## Exposure: a separate port, never the app port

**Do not mount the exporter as Rack middleware on the main app.** The ingress routes every path
of the main port, so `/metrics` would be public. Serve it from the same process on a port
nothing external routes:

```ruby
# config/puma.rb
# Prometheus metrics. The yabeda plugin reads Puma pool stats through the
# control app — a unix socket so no extra TCP port is exposed. The
# yabeda_prometheus plugin serves /metrics from this same process on a
# separate port (default 9394, override with PROMETHEUS_EXPORTER_PORT) that
# is never routed by Thruster or the ingress — cluster-internal only.
activate_control_app "unix://#{File.expand_path('../tmp/pumactl.sock', __dir__)}", no_token: true
plugin :yabeda
plugin :yabeda_prometheus
```

Notes that cost time to learn:

- `plugin :yabeda` **requires the control app** and raises at boot without it. The unix socket
  form adds no TCP surface; `no_token: true` is fine because the socket is filesystem-scoped to
  the pod.
- The exporter is a real `Puma::Server` (max 1 thread) started on `after_booted` — works on
  Puma 7 and 8.
- No auth on `/metrics` is correct *only because* the port is unreachable from outside the
  cluster: ClusterIP Service, no HTTPRoute/Ingress pointing at it. Keep it that way.
- Check the labels before calling it done: no user ids, no raw query strings, no PII. Label
  values are `controller/action/status/format/method` plus whatever `default_tag` you add —
  cardinality and privacy are the same review.
- Several projects on one dev machine will fight over 9394. Override with
  `PROMETHEUS_EXPORTER_PORT` locally; keep the default in deploy so the scrape config is uniform.

### Known wart: `status=""` on halted before_actions

When authentication halts the chain via `throw` (Devise/Warden), the `process_action` payload
has no status, and the series lands with `status=""` even though the client got a 302. It is one
extra series per controller, not a cardinality problem. Treat `status=""` as "halted before
action" in queries; don't try to patch it in the app.

## Temporal workers: the SDK already does this

If the service runs a Temporal worker, **do not hand-instrument it**. The Ruby SDK's Rust core
exposes a native Prometheus endpoint. Gate it on an env var so only the worker Deployment binds
the port:

```ruby
# bin/temporal-worker (or wherever the worker builds its client)
runtime = if (addr = ENV["TEMPORAL_METRICS_ADDR"])
  Temporalio::Runtime.new(
    telemetry: Temporalio::Runtime::TelemetryOptions.new(
      metrics: Temporalio::Runtime::MetricsOptions.new(
        prometheus: Temporalio::Runtime::PrometheusMetricsOptions.new(
          bind_address: addr,                # e.g. 0.0.0.0:9464
          counters_total_suffix: true,       # Prometheus naming conventions
          unit_suffix: true,
          durations_as_seconds: true
        )
      )
    )
  )
else
  Temporalio::Runtime.default
end
client = Temporalio::Client.connect(ADDRESS, NAMESPACE, runtime: runtime)
```

The endpoint serves an empty body until the worker registers pollers — that is not a bug.

## The cluster half: vmagent scrape

An unscraped endpoint is decoration. With the VictoriaMetrics operator and
`selectAllByDefault: true` on the VMAgent CR, per-namespace scrape CRs colocated with the app
manifest are picked up with no central config edit:

1. **Web**: name the exporter port on the container *and* the Service (the Service needs a
   label for the scrape selector — Service labels, not pod labels):

   ```yaml
   # Deployment container
   ports:
     - name: metrics
       containerPort: 9394
   # Service: add `labels: {app: <app>-web}` to metadata, and
   ports:
     - name: metrics
       port: 9394
       targetPort: 9394
   ```

   ```yaml
   apiVersion: operator.victoriametrics.com/v1beta1
   kind: VMServiceScrape
   metadata: {name: <app>-web, namespace: <ns>}
   spec:
     selector: {matchLabels: {app: <app>-web}}
     endpoints: [{port: metrics, path: /metrics}]
   ```

2. **Worker** (no Service): `VMPodScrape` with `podMetricsEndpoints: [{port: metrics}]`, where
   `metrics` is the containerPort name for 9464, and set `TEMPORAL_METRICS_ADDR: 0.0.0.0:9464`
   in the worker Deployment env.

Sequence the rollout: app PR (endpoint exists) → image bump → scrape CRs. A scrape CR that
lands before the image serves the port shows a permanently-down target, which trains you to
ignore down targets.

## Measurement validity: staging is not a small production

Endpoint plumbing transfers between environments; **memory measurements do not**, and the
contamination is invisible in the metrics themselves. Before quoting a heap-versus-RSS reading
from one environment as bearing on another, check for these (all real divergences that killed
one such reading here):

- `cache_store = :memory_store` puts the cache **inside the Ruby heap** — `gc_heap_live_slots`
  then measures the cache, not the suspected leak. Production on Solid Cache keeps it in
  Postgres.
- `queue_adapter = :async` runs jobs in the web process's thread pool — one image-processing
  job drops a large native-allocation spike into the middle of the trace. Production on Solid
  Queue does that work in a different pod.
- `limits_concurrency` is a Solid Queue feature and a **silent no-op under `:async`** — a
  staging run "verifying" throttled behaviour verifies the unthrottled version. Looks like
  validation, isn't.

Validate the pipeline wherever is cheapest; take memory readings only in an environment whose
cache store, queue adapter, and process topology match the one you're diagnosing — and label
any reading with the environment it came from.

## Verify end to end, once per service

- `kubectl port-forward` or in-cluster curl: `/metrics` returns `rails_requests_total` and
  `puma_*` series after at least one request.
- Hit a dynamic route with two different ids; confirm one series, not two.
- vmagent target healthy: VMUI target list, or `vmagent_scrape_pool_targets`.
- A real query returns data:
  `sum(rate(rails_requests_total[5m])) by (status)`.
