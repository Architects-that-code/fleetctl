# FleetCTL

A CLI to manage a fleet of OCI compute instances from a simple YAML configuration.

Status: v0.1.0 (bootstrapped; client and fleet operations are stubbed for now)

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
- --scale int          Scale fleet to desired total (stubbed)
- --rolling-restart    Perform rolling restart (stubbed)
- --auth-validate      Validate OCI authentication (performs a lightweight IAM call)
- --status             Print tracked fleet state from local store and exit
- --state string       Path to local state JSON (default ".fleetctl/state.json")
- --version            Print version and exit

Examples:
- Print summary:
  make run ARGS="--config fleet.yaml"
- Scale to 3 (stubbed behavior for now):
  make run ARGS="--config fleet.yaml --scale 3"
- Rolling restart (stubbed behavior for now):
  make run ARGS="--config fleet.yaml --rolling-restart"
- Auth validation:
  make run ARGS="--config fleet.yaml --auth-validate"

Exit Codes:
- 0: success
- 1: invalid arguments or configuration
- 2: infrastructure operation error (create/terminate/list etc.)
- 3: unexpected internal error

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
- spec.definedTags, spec.freeformTags: optional tag maps
- spec.instances[]: array of groups { name, count }

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
  - Scaling up adds records; scaling down marks records as terminated (LIFO by default).
  - Status prints a human-readable summary grouped by instance group.
- Examples:
  - make run ARGS="--config fleet.yaml --status"
  - make run ARGS="--config fleet.yaml --scale 3"
  - make run ARGS="--config fleet.yaml --status"
  - make run ARGS="--config fleet.yaml --scale 1"
  - make run ARGS="--config fleet.yaml --status"

Note: .fleetctl/ is excluded in .gitignore and should not be committed.

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
