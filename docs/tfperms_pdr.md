# tfperms — Product Requirements Document

**Status:** Draft
**Author:** Jim
**Last updated:** 2026-04-30

## Problem

Terraform configurations targeting Google Cloud Platform require a service account with sufficient IAM permissions to plan and apply. In practice, these service accounts are over-provisioned — granted broad predefined roles like `roles/editor` or `roles/owner` — because determining the actual minimum permission set for a given configuration is tedious and error-prone. The alternatives are worse: under-provisioned accounts produce cryptic 403 errors mid-apply, often leaving infrastructure in a partially-modified state.

Concretely: figuring out the permissions needed to apply a config with one Cloud Run service, one Cloud SQL instance, and one bucket means cross-referencing roughly thirty permission docs by hand, and the answer goes stale the moment someone adds a new resource type.

No existing tool statically analyzes a Terraform GCP configuration and reports the minimum IAM permissions required. The closest things in the space don't solve the problem:

- `iamlive` captures live AWS API calls — wrong cloud and runtime-only.
- `gcloud asset analyze-iam-policy` analyzes existing IAM policies, not what a config will need.
- `tfsec` and `checkov` lint policy violations; they don't infer required permissions from resource declarations.
- Google publishes "minimum permissions for Terraform" pages, but they're partial, manually maintained, and stale.

Authors who care about least-privilege resort to manual lookup against the GCP IAM permissions reference, which scales poorly and goes stale as configurations evolve. tfperms is also a viable on-ramp for the larger latent audience: teams currently shipping with `roles/editor` who would tighten up if it took one command instead of a research project.

## Users

The primary user is the **Terraform author writing GCP configurations** — typically a platform engineer, SRE, or developer responsible for infrastructure code. They invoke tfperms at write-time, in their editor or terminal, to answer the question: "What permissions does the service account running this configuration actually need?" Two distinct use modes share that question:

- The **mid-development author** runs tfperms repeatedly as a config grows. They care about delta and speed, and typically want the flat list output, not a custom role YAML.
- The **right-sizing SRE** runs it once or twice on a stable, inherited config. They care about correctness, and they want the role YAML they can pipe into `gcloud iam roles create`.

Secondary users are **platform engineers defining custom IAM roles** for Terraform automation pipelines. They use tfperms output as the seed for a custom role definition, replacing or refining a previously broad role grant.

A third audience is on the radar but not v1: the **security or compliance reviewer** who wants to answer "did this PR add any new permissions?" by diffing tfperms output between branches. That use case isn't built in v1, but it informs how stable the JSON output schema needs to be — see Epic 6.

tfperms is not built for runtime use. It does not validate permissions against a live GCP project, does not gate CI pipelines, does not enforce policy, and does not tell you whether a service account *currently* has the required permissions — it does not compare required vs. granted. Its job is to inform.

## Goals

1. Statically analyze a directory of Terraform files and report the minimum IAM permissions required **for the recognized resources**, with `terraform plan` and `terraform apply` reported separately. Unrecognized resources are reported alongside, not silently omitted.
2. Be honest about three distinct kinds of uncertainty, and never collapse them:
   - **Coverage uncertainty** — the resource type is not in the catalog.
   - **Resolution uncertainty** — an attribute is dynamic (interpolation, function call), so a conditional permission cannot be decided.
   - **Catalog correctness uncertainty** — the entry exists but may be wrong. v1 may not surface this directly in CLI output, but the catalog data structure carries provenance (cited source URL, last-verified date, verification method) from day one.
3. Produce output suitable for direct use: a flat permission list by default, a custom role YAML on demand.
4. Cover the most common GCP resources well in v1 rather than covering everything poorly.
5. Run fast enough to be invoked on every save or pre-commit (sub-second on typical configs). Without this, the mid-development use mode breaks.

## Non-goals

- Multi-cloud support. v1 is GCP-only; AWS and Azure are explicitly out of scope.
- Runtime permission validation against a live project.
- CI gate or policy enforcement engine. Sentinel and OPA exist for that.
- Terraform plugin or provider integration. tfperms is a standalone CLI.
- Consumption of `terraform plan` JSON output. v1 reads HCL directly.
- Full HCL expression evaluation — function calls, dynamic blocks, complex interpolations, or anything requiring a `terraform` runtime. Literal values, `variable` defaults, and `locals` blocks with literal RHS may be resolved when doing so meaningfully reduces noise; everything else is reported as unresolved.
- Resolution of remote, registry, or git-sourced modules.
- Resolution of `terraform_remote_state` data sources. These read another state file (locally or via a backend) and produce values the current config consumes; v1 treats them like remote modules — warn and skip.
- Reading, storing, or surfacing the values of attributes from variables marked `sensitive` or attributes documented as sensitive by the provider. Permission inference operates on attribute *presence and type*, not value content.
- Recommendation of predefined GCP roles. Role recommendation is an optimization problem with no single right answer — ~600 predefined roles, varying org-policy restrictions on which roles can be granted, and team-specific grant strategies. v1 outputs raw permissions and trusts the user to make the trade-off; users should not assume a v2 will solve this and should not design around that assumption.

## Success criteria

v1 is successful if:

- The catalog covers the 30-50 most commonly used `google_*` resource types, with the verification bar split into two tiers:
  - The top ~10-15 most-used resource types have **empirical verification** — a resource of that type has actually been created/updated/deleted using a permission-restricted SA holding only the catalog's claimed permission set, with the result documented.
  - The remaining ~20-35 resource types have **documentation-and-source verification** — cross-referenced against Google's IAM docs and the `terraform-provider-google` source, with the verification source URL/commit cited in the catalog YAML.
  - Catalog YAML schema includes a `verification` field per entry capturing method, source, and date.
- Running tfperms against the canonical test fixtures produces a permission list that, when granted to a service account, allows `terraform plan` and `terraform apply` to complete without IAM-related errors on the covered resources, and the granted permission set is, to a reasonable approximation, the minimum required — granted-but-unused permissions should not exceed a small constant beyond what is required.
- Canonical test fixtures live in `testdata/` and cover at least: single-file simple, multi-file with locals, module-using, IAM-binding-heavy, and mixed-API-surface configurations. Integration tests run against these.
- Unknown resources are reported clearly, with file and line context, and never silently omitted.
- The tool runs in under one second on a typical configuration (sub-1000 resource blocks), measured on a current developer workstation (e.g., M-series MacBook Pro), cold start, with no network calls.
- Common error cases (invalid path, parse error, malformed HCL) produce a single-line actionable message. The tool does not print Go stack traces or panic on user input.
- Distribution via `go install`, GoReleaser binaries, and a Homebrew tap is in place at first tagged release.

## User journeys

### Journey 1: Right-sizing an existing service account

Pat is an SRE who inherited a Terraform pipeline running as a service account with `roles/editor`. They want to replace it with a custom role containing only the permissions actually needed.

Pat runs:

```
tfperms ./infrastructure
```

The tool prints a flat list of plan permissions, a flat list of additional apply permissions, and a section listing two resources it didn't recognize. Pat reviews the unknowns and runs:

```
tfperms ./infrastructure --format=role --role-name=tf-pipeline
```

The tool emits a YAML custom role definition. Pat creates the role with `gcloud iam roles create`, and to cover the two unknown resources, retains a narrower predefined role (e.g., `roles/dataplex.admin`) layered on top. Pat swaps the service account bindings, reruns the pipeline, and files an issue with tfperms requesting catalog coverage for the unknown resources so future runs can collapse the layered role grant.

### Journey 2: Author checking a new module mid-development

A developer is writing a new Terraform module that provisions a Cloud Run service with a Cloud SQL backend. They run tfperms repeatedly as the module grows:

```
tfperms ./modules/api
```

Each run reports the current required permission set; because output is deterministic and sorted, the developer can compare runs across commits with `diff <(tfperms ./modules/api) <(git stash; tfperms ./modules/api; git stash pop)` to see what permissions the latest changes added. Before opening a PR, the developer adds the new permissions to the team's IAM-management Terraform config, and opens both PRs together. The JSON output is treated as a stable surface for this kind of cross-branch comparison.

### Journey 3: Catalog gap discovery

A user runs tfperms against a configuration using `google_dataplex_lake`, which isn't in the catalog. The tool reports the resource under "Unknown resources" with file and line. The user submits a PR adding `google_dataplex_lake` to `catalog/dataplex.yaml`, citing the GCP IAM reference URL for each permission and using `tfperms catalog scaffold google_dataplex_lake` to generate the YAML stub. The catalog test for the new entry passes; the PR merges; the next release ships with broader coverage.

### Journey 4: Investigating an unexpected permission

A user runs tfperms and sees `compute.instances.update` in the output but expected to see `compute.instances.create`. They rerun with `--by-resource`:

```
tfperms ./infrastructure --by-resource
```

The grouped output shows which resource block produced each permission, with file and line. The user identifies that one resource block has an attribute that triggered a conditional `update` permission in the catalog instead of `create`, investigates, and either updates the config or files a catalog issue if the conditional is wrong.

## Scope and milestones

### Epic 1: Project foundation

Establish the Go project structure, dependencies, and build tooling.

Stories:

- Initialize Go module and repository structure (`cmd/tfperms`, `internal/parser`, `internal/catalog`, `internal/resolver`, `internal/reporter`, `catalog/`).
- Pick a license. Recommendation: **Apache-2.0** — matches the HashiCorp ecosystem and provides explicit patent grant, which matters when the tool is used in commercial pipelines.
- Pin a minimum Go version in `go.mod` to a recent stable (e.g. Go 1.24).
- Add core dependencies: `hashicorp/hcl/v2`, `hashicorp/hcl/v2/hclsyntax`, `spf13/cobra` for CLI scaffolding.
- Set up GoReleaser config for darwin/linux/windows on amd64/arm64.
- Configure Homebrew tap repository and GoReleaser brew formula generation.
- Add Makefile or Taskfile for common dev workflows (build, test, lint, release-dry-run).
- Establish CI for build, tests, `golangci-lint`, `go mod tidy` check, and the catalog validator on push.

### Epic 2: HCL parser and resource extraction

Walk Terraform files and extract structural information about resource blocks.

Stories:

- Implement directory walker that finds all `.tf` files in a target path, respecting `.terraform/` and similar conventions.
- Implement HCL parser that extracts resource blocks, capturing resource type, resource name, source file path, and line number.
- Extract data source blocks alongside resources (data sources have plan-time read permission requirements).
- Define internal `Resource` struct with type, name, file, line, and a map of statically-resolvable attributes.
- Implement attribute extraction for the small set of attributes that gate conditional permissions (literal values; expressions left as unresolved markers).
- Build a static evaluation context: extract `variable` blocks (capturing literal `default` values) and `locals` blocks (capturing literal-RHS bindings), and use them when resolving attribute values. Without this, almost all conditional permissions resolve as unresolved.
- Define handling for `count`, `for_each`, and `dynamic` blocks: count permissions at the resource-block level (one create perm whether you make 1 or 10 instances); for `dynamic` blocks, parse the inner block type and ignore the iteration source. State this explicitly so the parser doesn't accidentally over-count.
- Add test fixtures covering single-file, multi-file, edge-case, commented-out, and `count`/`for_each`/`dynamic` configurations.

### Epic 3: Module resolution

Recursively parse local modules referenced from the root configuration.

Stories:

- Detect `module` blocks in parsed configurations and extract the `source` attribute.
- Classify module sources: local (`./`, `../`), registry (`hashicorp/...`), git, archive.
- Recursively parse local modules, accumulating resources from child modules into the root analysis.
- Propagate module-call arguments into the called module's static evaluation context. When a module is invoked with `source = "./mod"; project = "my-project"`, those literal argument values populate the called module's `variable` defaults so conditional permissions inside the module can resolve.
- Permissions are deduplicated across the full resource graph (root + all local module instances). Conditional permissions are taken as the union over all instances of a given resource type.
- Warn and skip non-local modules, surfacing them in the unknowns section of the report with their source string.
- Detect and prevent infinite recursion in pathological module graphs.
- Add test fixtures with nested local modules, mixed local/remote module trees, and modules called with literal arguments that gate conditionals inside the module.

### Epic 4: Permission catalog

Build the data layer that maps resource types to required permissions.

Stories:

- Define the catalog schema in YAML. Required fields per entry:
  - Per-resource `plan` / `create` / `update` / `delete` permission sets.
  - Optional `conditional` block keyed on attribute values.
  - `verification` — `{ method: empirical | docs+source, source_urls: [...], verified_at: <date>, verified_provider_version: <ver> }`.
  - `tested_against_provider` — provider version range against which this entry was verified, to flag drift.
  - A separate top-level shape for **data sources** (read-only, plan-time only).
  - Special handling for **IAM binding resources** (`google_*_iam_binding`, `_iam_member`, `_iam_policy`): these need `setIamPolicy` on a parent rather than CRUD on the resource itself. Schema captures the parent type so the resolver knows to map appropriately.
- Embed catalog YAML files into the binary using `go:embed`.
- Implement catalog loader and validator (schema check at build or test time, not runtime).
- Define and document the **catalog verification procedure**: empirical for the top ~10-15 most-used resources, docs+source for the remaining ~20-35, with cited evidence required in every catalog PR. Catalog updates without provenance are not accepted.
- Hand-curate entries for the top 30-50 GCP resource types. Initial target list sourced from public Terraform usage data and the author's own configurations. (Examples: `google_compute_instance`, `google_storage_bucket`, `google_bigquery_dataset`, `google_pubsub_topic`, `google_cloud_run_service`, `google_sql_database_instance`, etc.)
- Implement `tfperms catalog scaffold <resource-type>`: emits a YAML stub with all required fields filled in as TODOs. Lowers the bar for community contributions (see Journey 3).
- Implement catalog diagnostics — a test or subcommand that reports coverage by resource family, the oldest `verified_at` date, and entries missing provenance. Surfaces rot.
- Document the catalog contribution process in `CONTRIBUTING.md`, including how to verify a permission mapping against the GCP IAM reference and the terraform-provider-google source, with a worked example.
- Add per-resource catalog tests that lock the expected permission sets.

### Epic 5: Permission resolution

Combine extracted resources with the catalog to produce permission sets.

Stories:

- Implement resolver that takes a list of `Resource` structs and a catalog, returning three permission sets and a list of unknown resources:
  - `plan_perms` — dominated by `.get` permissions on resources that exist in state, used by `terraform plan`'s state refresh.
  - `apply_only_perms` — apply permissions that are not also plan permissions: typically `.create` / `.update` / `.delete`.
  - `total_apply_perms` — the union of the above; what an SA running `terraform apply` actually needs.
- Apply conditional permission rules where the static evaluation context (Epic 2) resolves the gating attributes. This story explicitly depends on the Epic 2 evaluation work; without it, conditionals are nearly all unresolved.
- Surface unresolved conditionals as warnings tied to the resource and attribute, with file:line context.
- Deduplicate permissions across the resource set.
- Honor `lifecycle { prevent_destroy = true }` when set as a literal: a resource with `prevent_destroy = true` does not contribute `delete` permissions, since `terraform apply` cannot delete it. Reduces false-positive permissions and rewards explicit lifecycle declarations.
- Provide a flag to include or exclude `delete` permissions from the apply set (default: include).

### Epic 6: Reporter and output formats

Render resolved permissions to user-facing output.

Stories:

- Implement flat permission list output as the default format. Prefix the list with a one-line summary (e.g. `42 permissions for 17 resources, 2 unknowns, 3 unresolved conditionals`) so the reader has instant context.
- Implement `--by-resource` grouped output for explanation and debugging. This is the primary path for Journey 4 (investigating unexpected permissions).
- Implement `--format=role` output that emits a gcloud-compatible custom role YAML, with a `--role-name` flag. Prepend a comment header with the literal `gcloud iam roles create ... --file=...` invocation so the user does not have to look it up.
- Implement `--format=json` output as a **versioned API surface**. The JSON schema includes a top-level `version` field; no breaking changes within a major version. This stability commitment is what makes branch-diffing (Journey 2) and a future PR-review use case (Users section) reliable.
- All output formats produce **deterministic, sorted output** — permissions sorted alphabetically, resources ordered by file:line. Required for `diff`-based workflows.
- Ensure unknown resources section appears in all formats, with file:line context.
- Ensure unresolved conditional warnings appear in all formats.
- Add a `--quiet` flag that suppresses warnings and unknowns (for users piping output into a role definition).

### Epic 7: CLI surface and ergonomics

Polish the user-facing command structure.

Stories:

- Define top-level `tfperms <path>` invocation with sensible defaults.
- Add flags: `--format` (flat|by-resource|role|json), `--role-name`, `--include-delete` / `--exclude-delete`, `--quiet`, `--version`.
- Add `--catalog-stats` to print coverage info from the Epic 4 diagnostics — useful for users curious whether their resources are covered without running against a config.
- Add `--check-version` to print the catalog's `tested_against_provider` set, so users can confirm the catalog matches their provider lockfile and don't file false-positive bugs that turn out to be version drift.
- Document the `--include-delete` / `--exclude-delete` trade-off in `--help`: include = sufficient for any apply (the default); exclude = minimum for the next apply assuming no destroys.
- Implement clear error messages for common failure modes (invalid path, parse errors, malformed HCL).
- Add `--help` output that explains the advisory framing and the v1 caveats (HCL only, local modules only, catalog coverage).
- Implement non-zero exit codes for parse failures only; unknown resources do not fail the command.

### Epic 8: Documentation and release

Make the project usable by people other than the author.

Stories:

- Write README with the problem statement, install instructions, usage examples, and the explicit non-goals.
- Add a "comparison to alternatives" section in the README: name `iamlive`, `gcloud asset analyze-iam-policy`, `tfsec`/`checkov`, and explain in one line each what tfperms does that they don't. Preempts the inevitable "isn't this just X?" comments.
- Document the catalog format and contribution flow, with a worked example of adding a new resource type.
- Ship issue and PR templates. A "request a resource type" issue template asking for resource name, GCP service, and an example use beats free-form issues for signal/noise.
- Show an example in the README of using tfperms with multi-project aliased providers, so users with that pattern don't assume the tool is the wrong fit.
- Write a short blog post or release announcement (Substack-appropriate) explaining the thesis and v1 limitations. Frame the catalog coverage limit explicitly and turn it into a contribution path rather than a flaw.
- Tag v0.1.0 and ship via GoReleaser.
- Verify Homebrew tap install works end-to-end.

## Risks and open questions

**Catalog accuracy is the entire product.** A wrong permission mapping is worse than a missing one — it teaches the user to trust output that's lying to them. Mitigation lives in Epic 4's verification procedure (split empirical / docs+source bar) and the Success-criteria minimality bar. Catalog PRs without provenance are not accepted.

**Catalog rot.** Distinct from initial-correctness risk: entries that were correct when written and silently went stale as Google's IAM model shifted. Over a long enough horizon, this is the dominant failure mode. Mitigation: catalog diagnostics surface entries with old `verified_at` dates and lowest-coverage resource families, so rot is visible rather than invisible.

**API-call-to-permission mapping is many-to-many.** The provider source shows API calls; mapping API call → IAM permission requires Google's docs (sometimes incomplete or wrong). Some API calls require multiple permissions (e.g., creating a service account often requires `iam.serviceAccounts.create` *and* `iam.serviceAccountKeys.create`); some permissions cover multiple API calls. Catalog YAML provenance must cite both the API call and the permission, and high-traffic entries must be empirically verified — without these, plausible-looking-but-wrong entries are easy to ship.

**Provider version drift.** The terraform-provider-google project changes its API call patterns over time; a permission required by v5 of the provider may differ from v4. Mitigation: declare an explicit "tested against provider version X.Y" in the README and per catalog entry. Don't pretend to be version-agnostic.

**Conditional permissions are a long tail.** Many GCP resources have permissions that depend on attribute values, and some of those attributes are dynamic. The "warn about unresolved conditionals" approach is honest but may generate noise. Mitigation: tune which conditionals are surfaced based on how often they actually matter in practice; suppress trivial cases. The static evaluation context (Epic 2) reduces the unresolved set materially but does not eliminate it.

**`terraform import` and `terraform state` operations are not analyzed.** v1 reports permissions for `plan` and `apply` only. Users running import flows or state manipulation will need additional read permissions; this is documented but not computed.

**Predefined-role-vs-permission tension.** GCP users often grant predefined roles, not raw permission lists. A user who reads tfperms output and tries to grant the listed permissions individually may find their org policy disallows it, or that role grants are administratively easier. v1 ships permissions only and lets the user map. v2 could optionally suggest the smallest-fit predefined role.

**Hand-curated catalog scaling.** 30-50 resources is achievable; 500 is not, by hand. The generator-from-provider-source approach (deferred from v1) becomes important once v1 ships and demand exceeds manual curation capacity.

**Open question: catalog distribution.** Should the catalog ship embedded in the binary only, or should tfperms also support an external `--catalog-path` override for users who want to extend it without forking? Lean: embedded only for v1; `--catalog-path` is a v1.1 addition once the catalog format stabilizes.

**Open question: handling of `provider` blocks with non-default project/region.** A configuration may target multiple projects via aliased providers. v1 treats permissions as project-agnostic (the permission set is the same regardless of which project it's granted in) — correct for the output, but the user-facing complement is "where to grant the role." The README shows an example of using tfperms output across multi-project aliased-provider setups so users don't assume the tool is the wrong fit.

**Open question: cross-project and org-level permissions.** Some resources need org-level or cross-project permissions to apply (e.g., creating a service account whose project differs from the resource's project requires permissions on both projects). v1 punts and treats permissions as project-internal; this is named explicitly as a known punt rather than a silent bug.

## Out of scope, considered for v2+

- Generator that derives catalog entries from terraform-provider-google source. The v1.x feature that scales the catalog past hand-curation; depends on v1's catalog format stabilizing.
- AWS and Azure providers.
- `terraform plan` JSON consumption mode. The benefit is **resolution of dynamic attributes** that v1 reports as unresolved (plan JSON has values resolved: variables interpolated, locals computed, module outputs filled in). The trade-off is that it requires a successful `terraform init` against a real backend — a much heavier prerequisite than reading HCL — so v1 stays HCL-only.
- Built-in permission-diff mode. `tfperms diff <ref>` comparing output between branches/refs cleanly. Currently served by `diff <(tfperms a) <(tfperms b)` against the stable JSON output; built-in support is the obvious v2 ergonomic upgrade.
- `terraform-docs`-style integration. A `tfperms doc` mode that emits a markdown table suitable for embedding in module READMEs. Mentioned now to lock in output-format stability as a v1 design constraint.
- `tfvars` file support. Loading `terraform.tfvars` and `*.auto.tfvars` would resolve more conditionals without needing `terraform init`. May be small enough to land in v1.x rather than v2; flagged here for evaluation.
- Predefined-role suggestion (smallest GCP role containing the required permission set).
- External catalog path / catalog plugins.
- Container image distribution. Users who need it for CI in the meantime can trivially build their own (`FROM scratch` plus binary copy).
- VS Code extension that runs tfperms on save and surfaces results inline. Contained scope, achievable as a v1.x project.
- LSP server with incremental analysis for real-time permission warnings as you type. A separate, larger project than the VS Code extension.
