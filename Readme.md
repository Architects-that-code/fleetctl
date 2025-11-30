# Why this why now?
 - had a customer that was not happy with existing tooling for our Instance Pools - wanted something that could do a rolling restart -   this ran around in my head thinking about how I would implement and what features I would want so I just started building (and then added some vibe coding in)
  - thoughts:
    - need a way to define fleet of instances
    - need a way to track state (local or live?)
    - need a way to do operations (scale, rolling restart)
    - need a way to see status (CLI, HTTP UI?)
    - need a way to authenticate (instance principal, user principal)
    - needs to be testable and extensible

# FleetCTL

A CLI to manage a fleet of OCI compute instances from a simple YAML configuration.

Status: v0.1.0 (auth validation, scale, and rolling restart implemented; use with caution)

## Quickstart

Prerequisites:
- Go 1.23+

Steps:
1) Copy the template and edit values:
   cp config/fleet.yaml.tmpl ./fleet.yaml
2) Build:
   make build
3) Run:
   ./bin/fleetctl --config fleet.yaml

Alternatively:
- make run ARGS="--config fleet.yaml"
- go run ./cmd/fleetctl --config fleet.yaml

Local development (user principal auth):
1) Copy the local template:
   cp config/fleet.local.yaml.tmpl ./fleet.local.yaml
2) Edit auth to point to your OCI CLI config/profile as needed.
3) Validate auth locally:
   make run ARGS="--config fleet.local.yaml --auth-validate"

Example output (current default behavior):
Fleet(kind=FleetConfig, name=dev-fleet, instances=1)

## CLI

Invocation rule: Requires --config plus at least one additional flag. If not provided, usage is printed and exit code 1.

Flags (modeled as flags for v0.1.x; may become subcommands later):
- --config string      Path to YAML config (default "fleet.yaml")
- --scale int          Scale fleet to desired total
- --rolling-restart    Perform rolling restart
- --auth-validate      Validate OCI authentication (performs a lightweight IAM call)
- --status             Print tracked fleet state from local store and exit
- --state string       Path to local state JSON (default ".fleetctl/state.json")
- --version            Print version and exit

Examples:
- Print summary:
  make run ARGS="--config fleet.yaml"
- Scale to 3 (performs real OCI operations):
  make run ARGS="--config fleet.yaml --scale 3"
- Rolling restart (performs real OCI operations):
  make run ARGS="--config fleet.yaml --rolling-restart"
- Auth validation:
  make run ARGS="--config fleet.yaml --auth-validate"

Exit Codes:
- 0: success
- 1: invalid arguments or configuration
- 2: infrastructure operation error (create/terminate/list etc.)
- 3: unexpected internal error

Important: Scale and rolling-restart perform real OCI operations (instance create/terminate). Use a sandbox compartment, verify shape/subnetId, and prefer fleet.local.yaml for local testing.

## Configuration

- Schema: schema/fleetctl.schema.json
- Template: config/fleet.yaml.tmpl
- Add YAML modeline to opt-in to the local schema (already present in templates and sample):
  # yaml-language-server: $schema=./schema/fleetctl.schema.json

Important fields (see docs/fleetctl-spec.md for full details):
- kind: "FleetConfig"
- metadata.name: fleet name
- spec.compartmentId: OCI compartment OCID
- spec.imageId: OCI image OCID
- spec.availabilityDomain: e.g., "PHX-AD-1"
- spec.shape: Compute shape (e.g., VM.Standard.E2.1.Micro)
- spec.subnetId: Subnet OCID for the primary VNIC
- spec.displayNamePrefix: optional prefix for instance display names
- spec.definedTags, spec.freeformTags: optional tag maps
- spec.instances[]: array of groups { name, count [, subnetId] }

Subnet selection precedence:
- instances[].subnetId for the matched group (if set)
- otherwise spec.subnetId

## Authentication

The client supports two auth methods configured in spec.auth:

- instance (default): Instance Principal auth (uses OCI instance metadata)
- user: User Principal via the OCI CLI config file and profile

Config fields:
- method: instance | user
- configFile (user only): path to OCI config, default ~/.oci/config
- profile (user only): profile name, default DEFAULT
- region (optional): explicit region override; otherwise uses OCI_REGION env or provider region

Examples:
- Instance principal (default):
  auth:
    method: instance
- User principal:
  auth:
    method: user
    configFile: "~/.oci/config"
    profile: "DEFAULT"
    region: "us-phoenix-1"  # optional

Note on Rancher Fleet extension warnings:
- If your editor associates files named "fleet.yaml" with Rancher Fleet schemas, the YAML modeline above forces the correct local schema.

### Auth validation troubleshooting

- If running locally and you see errors like "instance principal provider: ...", set `spec.auth.method: user` and configure a valid OCI CLI config:
  - `configFile`: path to your OCI config (e.g., `~/.oci/config`)
  - `profile`: profile name (e.g., `DEFAULT`)
- If using user principal and you see "OCI config file not found", ensure the `configFile` path exists and contains the specified `profile`.
- You can also set OCI_CLI_CONFIG_FILE to an explicit config path; ~ (tilde) and environment variables in configFile are supported.
- If the resolved region is empty or incorrect, set `spec.auth.region` or export `OCI_REGION` (e.g., `export OCI_REGION=us-phoenix-1`).
- To verify and print details, run:
  - `make run ARGS="--config fleet.local.yaml --auth-validate"`

## State Tracking

- Purpose: maintain a local ledger of instances created/terminated by fleetctl; not an authoritative OCI source of truth.
- Storage: JSON file at ".fleetctl/state.json" (override with --state).
- Scope: only tracks resources managed via this CLI on the local machine; does not discover pre-existing instances.
- CLI flags:
  - --status           Print tracked fleet state and exit
  - --state string     Path to local state JSON (default ".fleetctl/state.json")
- Behavior:
  - Scaling up adds records; scaling down marks records as terminated (FIFO — oldest first).
  - Status prints a human-readable summary grouped by instance group.
- Examples:
  - make run ARGS="--config fleet.yaml --status"
  - make run ARGS="--config fleet.yaml --scale 3"
  - make run ARGS="--config fleet.yaml --status"
  - make run ARGS="--config fleet.yaml --scale 1"
  - make run ARGS="--config fleet.yaml --status"

Note: .fleetctl/ is excluded in .gitignore and should not be committed.

## HTTP UI

Run fleetctl in HTTP mode to get a minimal web UI and SSE live updates.

Start the daemon:
- make run ARGS="--config fleet.local.yaml --http :8080 --reconcile-every 30s"
- Or: ./bin/fleetctl --config fleet.local.yaml --http :8080 --reconcile-every 30s

Endpoints:
- GET /               Minimal UI (status grid, badges, controls)
- GET /healthz        Liveness probe
- GET /status         Local vs Remote (OCI) comparison text
- GET /metrics        JSON metrics including control loop snapshot and action metrics
- GET /control        Control loop status JSON
- GET /events         Server-Sent Events stream used by the UI
- POST /scale         Body: {"desired": N}
- POST /rolling-restart
- POST /sync-state
- GET /openapi.json   OpenAPI 3.0 schema for the HTTP API

Badges (UI):
- Scaling badge (always visible):
  - Source: metrics.operation, metrics.phase, metrics.startTotal, metrics.targetTotal, metrics.launchSucceeded, metrics.terminateSucceeded.
  - Semantics:
    - "Scaling up to T (X of N)" when operation == "scale-up"; X = launchSucceeded, N = max(0, targetTotal - startTotal).
    - "Scaling down to T (X of N)" when operation == "scale-down"; X = terminateSucceeded, N = max(0, startTotal - targetTotal).
    - "Scaling idle" when operation is empty or phase == "done".
  - Note: The scaling badge reflects the active operation only. /scale requests are enqueued and do NOT change this badge until the operation actually starts under Fleet.opMu.
- Rolling Restart badge:
  - Source: metrics.operation == "rolling-restart", metrics.rollingRestartIndex and metrics.rollingRestartTotal.
  - Semantics: "Rolling restart i/total" while in progress; hidden when not active.
- Load Balancer badge:
  - Source: metrics.lbEnabled and metrics.lbBackends (optimistic updates during backend removals).
  - Semantics:
    - "LB disabled" when spec.loadBalancer.enabled is false.
    - "LB X backends" when enabled; X may optimistically decrement immediately when removals begin; final count is reconciled every loop tick and after scale/rolling-restart completes.
- Drift badge:
  - Source: control loop snapshot (ctrlStatus.desired vs ctrlStatus.actual).
  - Semantics: "No drift" when desired == actual; "Drift detected" otherwise.
- Scale queue badge:
  - Source: metrics.scaleQueue (serialized as [] when empty).
  - Semantics: Shows queued desired totals comma-separated, or "empty" when [].
  - Ordering and FIFO: New scale events are appended to the END of the queue by /scale; the head is popped only when Fleet.Scale begins (under opMu) and only if the desired matches the head (FIFO).
- Minimums badge:
  - Source: sum(spec.instances[].count) and per-group counts from the currently loaded config.
  - Semantics: Displays the config-file minimums used by the lower-bound scaling policy.

Badge ordering in the UI:
Fleet name, LB, Scaling, Rolling Restart, Drift, Scale queue, Minimums, then the status grid.

Control loop flow and states

Policy (only scale up automatically; lower-bound protection):
- desiredFromConfig = sum(spec.instances[].count)
- localBaseline = active count from the local state store
- target = max(desiredFromConfig, localBaseline)
- If actual < target: scale up to target
- If actual >= target: no automatic downscale
- Load balancer backends are reconciled every control loop tick

Flow per tick:
1) Reload configuration if the file changed and update ctrlStatus.lastConfigReload.
2) Compute desiredFromConfig and apply lower-bound (local baseline) to produce target; set ctrlStatus.desired.
3) Query OCI for actual instances; set ctrlStatus.actual and lastError.
4) If actual < target, invoke Fleet.Scale(target). Operations are serialized by Fleet.opMu. The scaling badge is driven solely by metrics set inside Scale().
5) Reconcile the load balancer to match the set of active instances.
6) Emit updated snapshots for /metrics, /control, and SSE UI.

Operation phases (metrics.phase):
- planning: metrics.Reset() was called for a new operation.
- launch: creating instances (scale-up) or after each terminate during rolling restart.
- terminate: deleting instances (scale-down) or per-step during rolling restart.
- verify: waiting until remote actual equals desired target.
- done: operation completed; metrics.operation cleared so the scaling badge returns to "Scaling idle".

Scale queue semantics:
- /scale enqueues the requested desired to metrics.scaleQueue and returns 202 immediately (fast path).
- The queue is FIFO: new events are added to the END; the head is popped only when Fleet.Scale begins (under opMu) and matches the head (metrics.PopScaleQueueIfHead).
- The scaling badge reflects only the active operation; queued values appear exclusively in the "Scale queue" badge.

Concurrency and safety:
- All mutating fleet operations (scale, rolling restart, LB changes) are serialized by Fleet.opMu.
- HTTP handlers avoid direct OCI list calls for counts; they consume control loop snapshots to prevent SDK retry races.
- LB metrics use optimistic decrement when removals begin and a post-operation reconcile to restore authoritative counts.

Notes:
- You can still downscale explicitly with POST /scale and a lower desired; this performs LB deregistration before terminating instances. The control loop itself will not automatically downscale below the local baseline.
- UI controls:
  - Set the "Desired total" and click "Scale" to trigger scale actions
  - "Rolling Restart" replaces instances one-by-one and updates LB backends accordingly
  - "Sync State" rebuilds the local store from OCI discovery

Examples:
- Scale to 3:
  curl -sS -X POST localhost:8080/scale -H 'Content-Type: application/json' -d '{"desired":3}'
- Rolling restart:
  curl -sS -X POST localhost:8080/rolling-restart
- Sync state:
  curl -sS -X POST localhost:8080/sync-state

## Development

Common targets:
- make tidy   # go mod tidy
- make build  # build to ./bin/fleetctl
- make run ARGS="--config fleet.yaml"
- make test
- make clean

Run directly:
- go run ./cmd/fleetctl --config fleet.yaml

Project layout:
- cmd/fleetctl/       Entry point
- internal/config/    Config parsing and types
- internal/client/    OCI client wrapper (stub)
- internal/fleet/     Fleet logic (stubs + summary)
- schema/             JSON schema for config
- config/             Config templates
- docs/               Documentation and specs (living)

## Functional Specification (Living Doc)

See: docs/fleetctl-spec.md

This document is the source of truth for current behavior, planned features, acceptance criteria, and non-functional requirements. It must be updated in PRs that change user-visible behavior (flags, commands, config schema).

## Roadmap (short)

- M0 (done): Bootstrapped CLI, config parsing, stubs, build tooling
- M1: Status/List command; align config struct with sample fields
- M2: Implement Scale with idempotency
- M3: Implement Rolling Restart
- M4: Config validation, richer Spec, tagging strategy
- M5: Tests, docs, examples, error taxonomy

## License

TBD
