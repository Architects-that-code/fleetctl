FleetCTL Functional Specification (Living Document)
Version: 0.1.0
Status: Draft (living, continuously updated)

Purpose
- Define the intended functionality, behavior, and constraints of the fleetctl CLI/daemon.
- Serve as the source of truth for current capabilities and upcoming work.
- Provide acceptance criteria and a shared vocabulary for contributors.

Scope
- Manage a fleet of OCI compute instances via a YAML config (fleet.yaml).
- Provide CLI commands to inspect state, scale, and perform rolling restarts.
- Provide an HTTP daemon mode for metrics/observability and remote control.
- Non-goals (for v0.x): multi-cloud support, rich GUI (beyond a basic HTML page), secret management beyond standard OCI mechanisms.

Current Implementation Snapshot (as of v0.1.0)
CLI entrypoint: cmd/fleetctl/main.go
- Invocation rule: Requires --config plus at least one additional flag (usage shown otherwise).
- Flags:
  - --config string (default: fleet.yaml) Path to fleet configuration file
  - --version Print version and exit
  - --status Print tracked fleet state from local store and exit
  - --state string Path to local state JSON (default ".fleetctl/state.json"; relocated next to config as .<fleet>.state.json unless overridden)
  - --auth-validate Validate OCI authentication by performing a lightweight API call (prints details on success)
  - --scale int Desired total instances (idempotent scale up/down)
  - --rolling-restart Serial one-by-one replacement of active instances
  - --sync-state Rebuild local state by discovering instances tagged to this fleet
  - --http string Start HTTP server (daemon mode), e.g., ":8080" or "127.0.0.1:8080"
  - --reconcile-every duration Background controller loop interval when --http is set (default 30s; e.g., 30s, 1m)

Configuration loader: internal/config
- config.ParseFile reads YAML into FleetConfig struct.
- Struct fields (subset):
  - kind, metadata.name
  - spec fields:
    - compartmentId (string)
    - imageId (string)
    - availabilityDomain (string) (Suffix or full name accepted; auto-resolved)
    - shape (string)
    - shapeConfig (object; required when Flex shapes used) { ocpus, memoryInGBs }
    - subnetId (string)
    - displayNamePrefix (string, optional)
    - scaling (object) { parallelLaunch, parallelTerminate } (required by schema; ints >= 1)
    - auth (object) { method: instance|user, configFile, profile, region }
    - definedTags (map[string]string), freeformTags (map[string]string)
    - instances (array): { name, count, subnetId? } (per-group overrides allowed)
- Subnet selection precedence:
  - instances[].subnetId (if set for the matched group), otherwise spec.subnetId

OCI Client: internal/client
- New(auth) initializes an OCI ConfigurationProvider based on spec.auth:
  - method: "user" uses OCI CLI config file/profile (path expansion, OCI_CLI_CONFIG_FILE supported)
  - method: "instance" uses Instance Principal
  - Region resolution: spec.auth.region -> OCI_REGION env -> provider.Region()
- Fleet tagging:
  - All launched instances include freeform tag fleetctl-fleet=<fleetName> for discovery.
- ValidateInfo(ctx) performs lightweight calls:
  - returns region, tenancy OCID, user OCID (if any), subscribed regions, regions count.
- Compute operations:
  - LaunchInstances(ctx, cfg, group, n)
    - AD auto-resolution; subnet/image preflight checks
    - If Work Request ID present, poll via Work Request API; otherwise poll instance lifecycle to RUNNING
    - Progress logs emitted during wait
  - TerminateInstances(ctx, ids)
    - Poll lifecycle to TERMINATED (NotAuthorizedOrNotFound treated as success during polling)
    - Progress logs emitted during wait
  - ListInstancesByFleet(ctx, compartmentId, fleetName)
    - Discover non-terminated instances by fleet tag

State store: internal/state
- JSON ledger file (default moved next to config as .<fleet>.state.json)
- API: AddActiveRecord, ActiveRecordsLIFO, MarkTerminatedByIDs, CountActive, Summary, ResetFleetActive (for SyncState)

Fleet logic: internal/fleet
- New(cfg, client, store) constructs Fleet
- Summary(): basic summary string of loaded config
- Scale(desiredTotal):
  - Scale Up:
    - Parallel launches with bounded concurrency: spec.scaling.parallelLaunch (default 5 if unset)
    - After launches complete, Verify phase checks actual (remote) equals desired; then SyncState to reconcile local ledger
  - Scale Down:
    - Parallel terminations with bounded concurrency: spec.scaling.parallelTerminate (default 10 if unset)
    - After terminations complete, Verify phase checks actual equals desired; then SyncState
- RollingRestart():
  - Strictly serial loop: terminate one -> wait -> mark terminated -> launch one -> wait -> record
- verifyActualMatches(ctx, desired):
  - Poll ListInstancesByFleet until actual equals desired or timeout
- SyncState():
  - Rebuild state store by listing instances via fleet tag and parsing groups from names

HTTP Daemon Mode
Start:
  ./bin/fleetctl --config fleet.yaml --http :8080 [--reconcile-every 30s]
Endpoints:
- GET /healthz
  - Liveness probe; returns "ok"
- GET /status
  - Prints text status (Local vs Remote counts, drift info, local summary)
- GET /metrics
  - JSON metrics; includes:
    - fleet: string
    - localActive: int
    - remoteActive: int
    - timestamp: RFC3339
    - control: object (control loop status; see below)
    - actions: object (operation metrics; see below)
- GET /control
  - JSON status of the background control loop; fields:
    - enabled: bool
    - interval: string (e.g., "30s")
    - lastTick: RFC3339 last loop tick time
    - lastConfigReload: RFC3339 last config reload time (if any)
    - desired: computed total from spec.instances[].count
    - actual: live count discovered via tag
    - lastAction: "scale to N" or "noop"
    - lastError: last loop error message, if any
    - loopCount: total iterations since start
- POST /scale
  - Body: { "desired": <int>=0+ }
  - Performs scale up/down with verification and SyncState
- POST /rolling-restart
  - Performs serial rolling restart
- POST /sync-state
  - Rebuild state store from discovery
- GET /openapi.json
  - OpenAPI 3.0 JSON for all endpoints
- GET /
  - Basic HTML page providing:
    - Status text
    - Metrics JSON
    - Control loop JSON
    - Scale/rolling-restart/sync-state controls
    - Link to OpenAPI JSON

Control Loops (Documented)
- Master Reconciliation Loop (daemon mode)
  - Trigger: runs every --reconcile-every (default 30s)
  - Steps:
    1) Reload config if mtime changed
    2) Compute desired total as sum(instances[].count)
    3) Discover actual total via tag
    4) If actual != desired:
       - Call Scale(desiredTotal)
       - Scale will perform parallel launches/terminations as needed, then verify + SyncState
    5) Record telemetry to /control and /metrics.control
- Scale Up Loop
  - Structure: parallel launch workers bounded by spec.scaling.parallelLaunch
  - Phases: planning -> launch -> verify -> done
  - Verification: poll actual until desired reached; then SyncState
  - Error handling: per-item capture; emits metrics.actions.launchFailed on error
- Scale Down Loop
  - Structure: parallel terminations bounded by spec.scaling.parallelTerminate
  - Phases: planning -> terminate -> verify -> done
  - Verification: poll actual until desired reached; then SyncState
- Rolling Restart Loop
  - Structure: strictly serial over the active list (LIFO selection)
  - Phases per-item: terminate -> launch
  - Guarantees: avoids outages by never replacing more than one at a time

Metrics and Observability
- Operation Metrics (internal/metrics; emitted via /metrics.actions)
  - Global snapshot fields:
    - operation: "scale-up" | "scale-down" | "rolling-restart" | "sync-state" | "verify"
    - phase: "planning" | "launch" | "terminate" | "verify" | "done"
    - startedAt: RFC3339
    - lastUpdate: RFC3339
    - launchRequested, launchSucceeded, launchFailed (ints)
    - terminateRequested, terminateSucceeded, terminateFailed (ints)
    - rollingRestartIndex, rollingRestartTotal (per-item progress)
    - lastError: last operation error string, if any
  - Emission points:
    - Scale Up:
      - Reset("scale-up"); phase "launch"; IncLaunchRequested(missing)
      - On each success: IncLaunchSucceeded()
      - On each failure: IncLaunchFailed(err)
      - After launch: phase "verify"; verifyActualMatches; then SyncState; Done()
    - Scale Down:
      - Reset("scale-down"); phase "terminate"; IncTerminateRequested(len(ids))
      - On each success: IncTerminateSucceeded()
      - On each failure: IncTerminateFailed(err)
      - After terminate: phase "verify"; verifyActualMatches; then SyncState; Done()
    - Rolling Restart:
      - Reset("rolling-restart"); SetRollingRestart(0, total)
      - For each item: SetRollingRestart(i+1, total), phase "terminate" -> "launch"; per-step counters; Done() at the end
- Control Loop Metrics (exposed via /control and included in /metrics.control)
  - See fields in HTTP Daemon Mode above

Acceptance Criteria (Observability)
- During scale-up/scale-down/rolling-restart:
  - /metrics.actions reflects current operation, phase, counts, and lastError when present
  - /status shows progress logs in CLI output; /metrics and /control reflect updates within 1s of changes
- In daemon mode:
  - /control returns enabled=true, correct interval, consistently advancing lastTick and loopCount
  - When config is modified, lastConfigReload updates and loop reacts accordingly
- After operations complete:
  - /metrics.actions.phase == "done"
  - Scale: actual (discovered) equals desired and state is reconciled (SyncState)
  - Rolling restart: all items processed and counts consistent

CLI Specification
- Exit codes:
  - 0: success
  - 1: invalid arguments or configuration
  - 2: infrastructure operation error (create/terminate/list etc.)
  - 3: unexpected internal error

Configuration Specification (fleet.yaml)
- kind (string): "FleetConfig"
- metadata
  - name (string)
- spec
  - compartmentId (string)
  - imageId (string)
  - availabilityDomain (string)
  - shape (string)
  - shapeConfig (when Flex shapes)
  - subnetId (string)
  - displayNamePrefix (string, optional)
  - scaling (object; REQUIRED by schema)
    - parallelLaunch (int >= 1)
    - parallelTerminate (int >= 1)
  - auth (object)
    - method: "instance" (default) or "user"
    - configFile (user only), profile (user only), region (optional)
  - definedTags (map[string]string)
  - freeformTags (map[string]string)
  - instances (array)
    - name (string)
    - count (int)
    - subnetId (string, optional)

Authentication
- Methods:
  - instance: Instance Principal (default)
  - user: OCI CLI config file/profile
- Region resolution:
  1) spec.auth.region
  2) OCI_REGION environment variable
  3) provider.Region()
- Config file resolution (user principal):
  1) spec.auth.configFile
  2) OCI_CLI_CONFIG_FILE
  3) ~/.oci/config

Examples
- Print summary
  - ./bin/fleetctl --config fleet.yaml
- Scale to 3 (real OCI operations; shows progress)
  - ./bin/fleetctl --config fleet.yaml --scale 3
- Rolling restart (serial)
  - ./bin/fleetctl --config fleet.yaml --rolling-restart
- Auth validate (prints details)
  - ./bin/fleetctl --config fleet.local.yaml --auth-validate
- HTTP daemon (metrics + control)
  - ./bin/fleetctl --config fleet.local.yaml --http :8080 --reconcile-every 30s
  - curl -s localhost:8080/metrics | jq
  - curl -s localhost:8080/control | jq
  - open http://localhost:8080/

Behavioral Requirements (to implement/harden)
- Idempotent Scale:
  - Running twice with same N makes no changes the second time
- Safe deletion policy for downscaling (deterministic: LIFO based on state)
- Respect OCI rate limits and backoff
- Rolling Restart:
  - Strictly serial; configurable batch size/window in future
- Config Validation:
  - Validate required fields (shapeConfig for Flex, scaling block) before operations
- Observability:
  - Structured logs and metrics (see ActionsMetrics and control status)

State Tracking
- Purpose: Maintain a local ledger of instances created/terminated by fleetctl
- Storage: JSON file adjacent to config (.fleetName.state.json) unless overridden via --state
- Sync / Discovery:
  - SyncState rebuilds ledger from OCI by tag
- Operational notes:
  - Recommend not committing local state files; add to .gitignore

Non-Functional Requirements
- Reliability: backoff and retries where appropriate; idempotent operations
- Performance: bounded concurrency; parallelism tuned by config
- Security: OCI-standard auth; avoid logging secrets
- UX: clear error messages with remediation hints
- Testing: unit tests for config parsing and decisions; integration tests where possible

Change Log
- 2025-11-27
  - Added HTTP daemon with /healthz, /status, /metrics, /control, /scale, /rolling-restart, /sync-state, and /openapi.json
  - Implemented master reconciliation loop (--reconcile-every) with config reload and drift correction
  - Added fleet tagging, SyncState, and default state file co-located with config
  - Added configurable bounded concurrency (spec.scaling.parallelLaunch/parallelTerminate)
  - Added operation metrics (/metrics.actions) and control loop status (/control)
- 2025-11-26
  - Implemented real Scale and RollingRestart
  - Added spec fields: shape, subnetId, displayNamePrefix; schema and templates updated
  - Extended state store and client
- 2025-11-23
  - Enhanced --auth-validate to return and print details
- 2025-11-22 v0.1.0 (draft)
  - Initial spec created; roadmap and acceptance criteria outlines
