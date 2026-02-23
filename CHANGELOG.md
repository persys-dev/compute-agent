# Changelog

## 2026-02-23 (Unreleased)

Source: `git diff` inside `compute-agent`

### Incremental Update (Latest)

#### Summary

- Made metrics server port configurable via `PERSYS_METRICS_PORT` (default `8089`) instead of a hardcoded port.
- Added `compute-agent/sample.env` with baseline runtime, TLS, scheduler, and metrics environment variables.
- Fixed stale terminal-retry state for workloads whose desired state is `Stopped`:
  - normalize `Failed` runtime state to `Stopped` when desired is stopped,
  - clear retry/terminal metadata and reset retry tracker for stopped workloads,
  - prevent failed/terminal retry branch from running unless desired state is `Running`.
- Updated README config table to document `PERSYS_METRICS_PORT`.

#### Changed Files (Latest)

1. `compute-agent/internal/config/config.go`

- Added `MetricsPort` to config model.
- Added env parsing: `PERSYS_METRICS_PORT` with default `8089`.
- Added config validation for metrics port range.

1. `compute-agent/cmd/agent/main.go`

- Replaced hardcoded metrics server address with `fmt.Sprintf(":%d", cfg.MetricsPort)`.

1. `compute-agent/sample.env` (new file)

- Added sample environment values for:
  - server ports (`PERSYS_GRPC_PORT`, `PERSYS_METRICS_PORT`),
  - TLS/Vault,
  - runtime toggles,
  - scheduler endpoint and node metadata.

1. `compute-agent/internal/workload/manager.go`

- Added `normalizeStateForDesired(...)` for desired-state-aware status normalization.
- Added `clearRetryStateMetadata(...)` to remove stale retry/terminal keys.
- Applied normalization in `GetStatus`, `ReconcileWorkload`, and desired-state transition path.
- Cleared retry state/reset tracker when workload desired state is `Stopped`.
- Scoped failed-recovery reconciliation branch to `desired=running` only.

1. `compute-agent/README.md`

- Documented `PERSYS_METRICS_PORT` in configuration table.

### Summary

- Added agent-side telemetry initialization and gRPC trace context propagation.
- Added queue-level metrics hooks and Prometheus task metrics.
- Improved status reporting for in-flight tasks to reduce stale-state reapply loops.
- Improved VM status diagnostics (state/reason mapping + metadata).
- Added pending-state recovery policy (restart then delete fallback).
- Added reconcile start retry metadata/backoff deferral behavior.
- Fixed restart safety path: same-revision desired-state transitions now do runtime start/stop without recreate.
- Expanded proto generation outputs to shared package paths.
- Documented retry/backoff behavior in README.

### Changed Files (exact)

1. `compute-agent/Makefile` (+10 / -0)

- Updated `proto` target to also generate stubs into shared packages:
  - `../pkg/agent/api/v1`
  - `../pkg/agent/control/v1`

1. `compute-agent/README.md` (+43 / -0)

- Added `Retry and Backoff Strategy` section documenting:
  - scheduler reconnect backoff,
  - local reconcile retry policy,
  - pending-timeout recovery flow.

1. `compute-agent/cmd/agent/main.go` (+15 / -0)

- Added OpenTelemetry setup on startup via `internal/telemetry.Setup`.
- Added OTel shutdown on process exit with timeout.
- Wired task queue metrics observer when metrics are enabled.

1. `compute-agent/go.mod` (+8 / -2)

- Added direct OTel dependencies:
  - `go.opentelemetry.io/otel`
  - `go.opentelemetry.io/otel/sdk`
  - `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`
- Added/updated related indirect deps for OTLP and grpc-gateway proto stack.

1. `compute-agent/internal/control/client.go` (+60 / -2)

- Added unary gRPC client interceptor for OpenTelemetry spans.
- Added trace context injection into gRPC metadata.
- Applied interceptor to both insecure and TLS dial paths.

1. `compute-agent/internal/grpc/server.go` (+69 / -2)

- Added unary gRPC server interceptor for OpenTelemetry spans.
- Added inbound trace context extraction from gRPC metadata.
- Updated `GetWorkloadStatus` to overlay pending/running task metadata on top of stored status:
  - returns `ACTUAL_STATE_PENDING` for in-flight task states,
  - carries task metadata/message/updated_at so callers see live in-flight state.

1. `compute-agent/internal/metrics/metrics.go` (+67 / -0)

- Added task queue metrics:
  - `persys_agent_task_queue_depth` gauge,
  - `persys_agent_task_latency_seconds` histogram,
  - `persys_agent_task_executions_total` counter,
  - `persys_agent_task_failures_total` counter.
- Added helper methods:
  - `SetTaskQueueDepth`
  - `ObserveTaskExecution`

1. `compute-agent/internal/runtime/vm.go` (+162 / -10)

- VM `Status` now reports richer messages with domain state reasons.
- Paused domains now map to `ActualStatePending` (instead of `Stopped`) to avoid destructive upper-layer reactions.
- Added detailed VM metadata in `StatusMetadata`:
  - `vm.domain_state`, `vm.domain_state_code`,
  - `vm.domain_reason`, `vm.domain_reason_code`,
  - network lookup error metadata when interface discovery fails.
- Added helper mappings:
  - `vmDomainStateName`
  - `vmDomainReasonText` (libvirt reason enums for paused/shutoff/running/shutdown/crashed/etc.)

1. `compute-agent/internal/task/queue.go` (+39 / -0)

- Added `MetricsObserver` interface.
- Added observer wiring to queue lifecycle:
  - emit queue depth on start/submit/worker consume,
  - emit per-task duration + failure/success on completion.

1. `compute-agent/internal/workload/manager.go` (+110 / -4)

- Added pending recovery constants and metadata keys:
  - `pending_since`, `pending_recovery_action`, `pending_recovery_reason`, `pending_recovery_deleted`
  - threshold `5m`.
- Removed immediate delete-on-start-failure from `ApplyWorkload` path.
- Added `handlePendingRecovery` in reconcile path:
  - if pending > 5m: attempt stop+start,
  - if restart fails: delete runtime workload, mark failed, persist desired state `Stopped` to avoid loop.
- Added retry deferral for failed starts in reconciliation:
  - records `retry_attempts`, `next_retry_time`, `failure_reason`, `last_error`,
  - defers retry until tracker allows,
  - clears retry metadata on successful start.
- Updated same-revision apply behavior:
  - no longer blindly skips when desired state changed,
  - applies desired-state transitions with runtime `Start`/`Stop`,
  - avoids recreate/delete for running<->stopped transitions (critical restart safety fix).

1. `compute-agent/internal/workload/manager_test.go` (+new tests)

- Added regression tests proving same-revision desired-state transitions do not call recreate/delete:
  - `TestApplyWorkload_SameRevision_DesiredStateChange_StopWithoutRecreate`
  - `TestApplyWorkload_SameRevision_DesiredStateChange_StartWithoutRecreate`

1. `compute-agent/examples/client/specs/vm-spec.json` (+1 / -1)

- Changed disk `size_gb` from `10` to `5`.

1. `compute-agent/internal/telemetry/otel.go` (new file, +76)

- New OpenTelemetry bootstrap package:
  - reads exporter endpoint from env,
  - configures OTLP HTTP trace exporter,
  - sets tracer provider and W3C trace/baggage propagators,
  - returns shutdown function for graceful termination.

### Diff Stats

- Total changed tracked files: 11
- Total added lines in tracked files: 584
- Total removed lines in tracked files: 21
- New untracked file: 1 (`internal/telemetry/otel.go`, 76 lines)
