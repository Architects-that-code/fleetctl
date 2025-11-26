FleetCTL Functional Specification (Living Document)
Version: 0.1.0
Status: Draft (living, continuously updated)

Purpose
- Define the intended functionality, behavior, and constraints of the fleetctl CLI.
- Serve as the source of truth for current capabilities and upcoming work.
- Provide acceptance criteria for features and a shared vocabulary for contributors.

Scope
- Manage a fleet of OCI compute instances via a YAML config (fleet.yaml).
- Provide CLI commands to inspect state, scale, and perform rolling restarts.
- Non-goals (for v0.x): multi-cloud support, GUI, secret management beyond standard OCI mechanisms.

Current Implementation Snapshot (as of v0.1.0)
- CLI entrypoint: cmd/fleetctl/main.go
  - Flags:
    - --config string (default: fleet.yaml)
    - --scale int (desired total instances, stubbed)
    - --rolling-restart (bool, stubbed)
    - --auth-validate (bool) validate auth and print details
    - --status (bool) print tracked fleet state and exit
    - --state string path to local state JSON (default ".fleetctl/state.json")
    - --version (prints version)
  - Default behavior: loads config and prints a human-friendly summary (when no action flags).
- Config loader: internal/config
  - config.ParseFile reads YAML into FleetConfig struct.
  - Struct currently includes: kind, metadata.name, spec.{compartmentId, imageId, definedTags, freeformTags, instances[]}
  - Struct also includes spec.availabilityDomain.
- Client: internal/client
  - New(auth) initializes an OCI ConfigurationProvider based on spec.auth:
    - method: "user" uses OCI CLI config file/profile
    - method: "instance" uses Instance Principal
    - Region resolution: spec.auth.region -> OCI_REGION env -> provider.Region()
    - User principal config path: spec.auth.configFile -> OCI_CLI_CONFIG_FILE env -> ~/.oci/config
  - ValidateInfo(ctx) performs lightweight calls to validate auth and returns:
    - resolved region
    - tenancy OCID (if available)
    - user OCID (if user principal)
    - subscribed regions (if available)
    - total regions available
  - --auth-validate uses ValidateInfo and prints these details
  - Client is lazily initialized only when an action flag requires OCI access.
- Fleet logic: internal/fleet
  - New(cfg, client) constructs Fleet.
  - Summary() returns a string summary of the loaded config.
  - Scale() and RollingRestart() are stubbed and log requested actions.
- Build/Run
  - make build produces ./bin/fleetctl
  - make run ARGS="--config fleet.yaml" runs the CLI
  - go run ./cmd/fleetctl also supported

CLI Specification
Invocation rule: Requires --config plus at least one additional flag; otherwise usage is printed and the program exits with code 1.
- Global flags
  - --config string
    - Default: fleet.yaml
    - Purpose: path to YAML config describing the fleet
    - Errors:
      - File not found: exit non-zero; log fatal
      - YAML invalid: exit non-zero; log fatal
  - --version
    - Prints CLI version and exits 0
  - --status
    - Prints tracked fleet state from local store and exits 0
  - --state string
    - Path to local state JSON file (default ".fleetctl/state.json")
  - --auth-validate
    - Validate OCI authentication by performing a lightweight IAM call; exits 0 on success, non-zero on failure
- Commands (v0.1.x modeled as flags; may evolve into subcommands later)
  - --scale N
    - Desired behavior:
      - Set total number of instances across the fleet to N (idempotent)
      - If current total < N, create missing instances according to instance groups
      - If current total > N, terminate excess instances (policy: reverse-creation order or lexicographic, TBD)
    - Pre-conditions:
      - Config valid; OCI credentials available
    - Post-conditions:
      - Total instances equal N
    - Errors:
      - Provisioning/termination errors should produce non-zero exit; partial progress may exist
  - --rolling-restart
    - Desired behavior:
      - Restart instances one-by-one (or batched with window size), waiting for readiness between steps
    - Pre-conditions:
      - Config valid; OCI credentials available; ability to determine instance readiness
    - Post-conditions:
      - All targeted instances replaced/restarted
    - Errors:
      - Non-zero exit if any step fails; should avoid thundering herd (respect backoff or delays)

Exit Codes
- 0: success
- 1: invalid arguments or configuration
- 2: infrastructure operation error (create/terminate/list etc.)
- 3: unexpected internal error

Configuration Specification (fleet.yaml)
- kind (string)
  - Example: FleetConfig
  - Validation: must equal FleetConfig (TBD strictness)
- metadata
  - name (string) — identifier for the fleet
- spec
  - compartmentId (string) — OCI compartment OCID
  - imageId (string) — OCI image OCID
  - availabilityDomain (string) — e.g., PHX-AD-1
  - auth (object)
    - method: "instance" (default) or "user"
    - configFile (user only): path to OCI config, default ~/.oci/config
    - profile (user only): profile name, default "DEFAULT"
    - region (optional): region override; else uses OCI_REGION env or provider region
  - definedTags (map[string]string) — optional
  - freeformTags (map[string]string) — optional
  - instances (array of InstanceSpec)
    - name (string) — logical name/group identifier
    - count (int) — baseline count for the group
Notes:
- Additional fields will be added as needed (shape, subnet, VCN, metadata, boot volume size, etc.).
- Unknown fields should be preserved in doc/spec and added to struct as we implement.

Authentication
- Methods:
  - instance: Instance Principal (default) via OCI metadata auth
  - user: User Principal via OCI CLI config file and profile
- Config (spec.auth):
  - method: "instance" or "user"
  - configFile (user only): path to config file (default ~/.oci/config)
  - profile (user only): profile name (default "DEFAULT")
  - region (optional): overrides region; else uses OCI_REGION env or provider region
- Region resolution:
  1) spec.auth.region
  2) OCI_REGION environment variable
  3) provider.Region() from auth provider
- Config file resolution (user principal):
  1) spec.auth.configFile
  2) OCI_CLI_CONFIG_FILE environment variable
  3) ~/.oci/config

Examples
- Print summary
  - make run ARGS="--config fleet.yaml"
  - Output: Fleet(kind=FleetConfig, name=dev-fleet, instances=1)
- Scale to 3 (stubbed behavior today)
  - make run ARGS="--config fleet.yaml --scale 3"
- Rolling restart (stubbed behavior today)
  - make run ARGS="--config fleet.yaml --rolling-restart"
- Auth validate (prints details)
  - make run ARGS="--config fleet.yaml --auth-validate"
  - For local development (user principal):
    make run ARGS="--config fleet.local.yaml --auth-validate"
  - Output (example):
    Auth validation succeeded
      region: us-phoenix-1
      tenancy: ocid1.tenancy.oc1...
      user: ocid1.user.oc1... or (instance principal)
      regions_available: 34
      subscriptions: us-phoenix-1,us-ashburn-1

Behavioral Requirements (to implement)
- List/Status (optional milestone)
  - List current instances (ID, name, lifecycle state, tags) filtered by fleet identifier/tags
  - Acceptance:
    - Returns 0 on success; shows accurate, current state
- Scale
  - Idempotent: running twice with same N makes no changes the second time
  - Safe deletion policy for downscaling (deterministic; documented)
  - Respect OCI rate limits and backoff
  - Acceptance:
    - After success, total instances equals N; no orphaned instances
- Rolling Restart
  - Replace instances one-by-one (or in batches), waiting for readiness
  - Configurable batch size and delay
  - Acceptance:
    - No more than batch size replaced concurrently
    - Respect failure thresholds; halt on repeated failures with clear error
- Config Validation
  - Validate required fields before starting operations; fail fast with concise messages
- Observability
  - Structured logs (level, field context); progress updates for long ops

State Tracking
- Purpose: Maintain a local ledger of instances created/terminated by fleetctl; not an authoritative OCI source of truth.
- Storage: JSON file at ".fleetctl/state.json" (override via --state).
- Scope: Only tracks resources managed via this CLI on the local machine; does not discover pre-existing instances.
- API (internal/state): AddActiveInstances, RemoveActiveInstances, CountActive, Summary.
- Behavior:
  - scale N updates tracked state to match N
  - --status prints a human-readable summary grouped by instance group
  - Future milestones will reconcile tracked state with OCI and detect drift
- Operational notes:
  - Do not commit .fleetctl/ to source control; add it to .gitignore
  - Deleting the state file forgets history; use cautiously

Non-Functional Requirements
- Reliability: retries with backoff for OCI calls; idempotent operations
- Performance: reasonable parallelization within rate limits
- Security: use OCI-standard auth; avoid logging secrets
- UX: clear error messages with remediation hints
- Testing: unit tests for config parsing and fleet decisions; integration tests behind a feature flag or separate profile

Roadmap & Milestones
- M0 (done): Bootstrapped CLI, config parsing, stubs, build tooling
- M1: Status/List command; align config struct with sample fields (availabilityDomain)
- M2: Implement Scale with idempotency; deterministic downscale policy
- M3: Implement Rolling Restart with batch/window size and readiness checks
- M4: Config validation, richer Spec (shape, subnet, etc.), and tagging strategy
- M5: Tests, docs hardening, examples, error taxonomy and exit codes enforced

Open Questions
- Downscale policy: which instances to terminate first? (age, name sort, explicit policy)
- Readiness signal: how to determine instance readiness? (lifecycle state, custom health check, cloud-init logs)
- Instance grouping: allocate scaling across groups or treat total as global with weights?
- Tagging strategy: which tags uniquely identify fleet membership across regions/compartments?

Risks and Assumptions
- OCI rate limiting can throttle operations; must implement backoff and chunking
- Partial failure scenarios must be handled and reported clearly
- Config drift vs. actual state requires reconciliation logic

Change Log
- 2025-11-22 v0.1.0 (draft)
  - Initial spec created. Mirrors current CLI flags and stubs. Added roadmap and acceptance criteria outlines.
- 2025-11-23
  - Enhanced --auth-validate to return and print details (region, tenancy, user, region subscriptions).
  - Updated README and Current Implementation Snapshot with --status, --state, --auth-validate.

Maintenance Policy
- Update this document with any user-visible behavior changes (flags, commands, config schema) as part of each PR.
- Keep Roadmap and Change Log current to reflect planned and completed work.
