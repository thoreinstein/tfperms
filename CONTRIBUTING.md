# Contributing to tfperms

This guide focuses on the area most contributions land in: the GCP
permission catalog at `catalog/*.yaml`. Code-level contributions follow
the conventions visible in `internal/parser/` and the existing tests;
the catalog has its own conventions and they are documented here.

If you are short on time, jump to the [pull request checklist](#pull-request-checklist)
at the bottom — it is the canonical list of gates a catalog PR must
pass. The sections above explain why each gate exists.

## What the catalog is

`catalog/*.yaml` is the source of truth that maps Terraform resource
types onto the GCP IAM permissions tfperms reports as required for
plan and apply. Each YAML file is a partition by GCP service family
(`storage.yaml`, `compute.yaml`, ...). All files merge into a single
in-memory lookup at startup.

The schema and loader live in `internal/catalog/`. You almost never
need to read the Go code to add a catalog entry — the YAML is enough.

## Adding a new service file

1. Pick a service-level filename (e.g. `pubsub.yaml`). Catalog files
   MUST use the `.yaml` extension — `.yml` is rejected. The embedded
   `//go:embed` pattern in `catalog/embed.go` only matches `*.yaml`,
   so a `*.yml` file would work locally via the disk loader but
   silently vanish from the production binary; the loader rejects the
   extension up front to keep the two in lockstep.
2. Generate a stub with `tfperms catalog scaffold <terraform_type>`.
   The command derives the service-level filename from the resource
   type's `google_<service>_*` prefix, creates the file if it does
   not exist, and writes a YAML entry with every required schema
   field pre-populated with `TODO` sentinels. Use `--data-source` for
   `data_sources:` entries and `--iam-binding` for IAM resources;
   without a flag the stub lands under `resources:`. The TODO
   sentinels are deliberate — they parse as valid YAML so the file
   loads, but the validator rejects them so the contributor cannot
   accidentally land an unverified entry.
3. Copy structural details from `catalog/storage.yaml` when in doubt.
   It is intentionally the canonical example; every schema feature is
   exercised at least once (resources, data_sources, iam_bindings,
   conditionals).
4. Run `make catalog-validate`. This wraps the schema and repository
   consistency tests; a new file with a missing field, an unknown
   verification method, a dangling `parent_resource`, or a TODO
   sentinel left in place will fail there with a `<file>:<line>`
   pointer to the offending node.

## Catalog verification procedure

Every catalog entry MUST carry provenance. The `verification` block
records four required fields — `method`, `source_urls`, `verified_at`,
and `verified_provider_version` — and updates without them are
rejected by the validator and by review. `verified_at` is a
`YYYY-MM-DD` date that must fall inside a fixed window enforced by
`TestRepositoryCatalogVerificationProvenanceComplete` in
`internal/catalog/repo_test.go`:

- **Floor:** on or after **2024-01-01** (`provenanceFloorDate`, the
  catalog's project floor — chosen as a fixed wall-clock-independent
  lower bound so the test is deterministic across CI runs).
- **Ceiling:** at most **seven days** in the future relative to the
  test's wall clock at run time (`provenanceCeilingSlack`). The slack
  exists to absorb timezone differences between a contributor's
  machine and CI; it is **not** a recency requirement on past dates.

In other words: any past date from 2024-01-01 onward will pass the
automated test, including dates from previous years. Reviewers — not
the validator — are responsible for catching stale entries where the
provider has moved on. Write the actual date the verification ran;
do not back-date or forward-date.

Obvious stub dates (`0001-01-01`, `1970-01-01`, `2000-01-01`,
`2099-01-01`, `9999-12-31`) are rejected explicitly via the
`obviouslyStubVerifiedAt` map. If you genuinely need to update an
entry, re-run the verification and write today's date.

The two accepted verification methods are:

- **`empirical`** — you ran `terraform apply` against a service
  account whose role you tightened until each missing-permission error
  guided you to the next entry. Reserved for the most-used resources
  where false-positives hurt the most. Cite the actual commands and
  the dates you ran them.
- **`docs+source`** — you cross-referenced the resource implementation
  in [terraform-provider-google][1] against the [GCP IAM permission
  reference][2]. Cite the exact file in the provider and the exact
  service permissions page.

[1]: https://github.com/hashicorp/terraform-provider-google
[2]: https://cloud.google.com/iam/docs/permissions-reference

### Verification tiers

The Epic 4 PDR splits catalog entries into two tiers:

- **Top-tier (~10-15 resources)**: empirically verified. These are
  the most-used resource types where a wrong permission mapping does
  the most damage. The PDR's example top-tier list is
  `google_compute_instance`, `google_storage_bucket`,
  `google_bigquery_dataset`, `google_pubsub_topic`,
  `google_cloud_run_service`, `google_sql_database_instance`, plus
  the rest of the Top-15.
- **Long-tail (~20-35 resources)**: documentation-and-source
  verified. The remaining hand-curated coverage uses the `docs+source`
  workflow above.

Tier membership is tracked in
`internal/catalog/repo_test.go` as the `topTierEmpiricalResources`
map. Two tests enforce a bidirectional contract between the map and
the YAML:

- `TestRepositoryCatalogTopTierResourcesAreEmpirical` requires every
  resource on the map to have `method: empirical` in YAML.
- `TestRepositoryCatalogEmpiricalEntriesAreOnTopTierList` requires
  every resource with `method: empirical` in YAML to appear on the
  map.

To promote a resource into the empirical tier in a single PR:

1. Run the empirical verification against a real GCP project. Capture
   the exact `gcloud iam service-accounts ...` and `terraform apply`
   command lines plus their dates in the entry's verification block
   comments.
2. Change the YAML entry's `verification.method` to `empirical` and
   update `verified_at` to the date the verification ran.
3. Add the Terraform type to `topTierEmpiricalResources` with a
   one-line rationale (e.g. `"PDR Epic 4 top-15 example"`). The map
   value is surfaced verbatim in test failures, so a future maintainer
   reading a CI log understands why the resource is on the list.
4. Run `go test ./internal/catalog/...`. Both tier tests should now
   pass.

The map is intentionally empty in the schema-and-loader baseline. As
PRs do empirical verification work the map grows; the test pair makes
silent drift impossible.

For each entry you add or modify:

1. Pick the verification method. If you are not running `terraform
   apply` against a real GCP project, the method is `docs+source`.
2. Open the provider source for the resource. For
   `google_storage_bucket` that is
   `google/services/storage/resource_storage_bucket.go`. Note every
   distinct API call (`Buckets.Get`, `Buckets.Insert`, `Buckets.Patch`,
   `Buckets.Delete`, `Buckets.GetIamPolicy`, `Buckets.SetIamPolicy`).
3. Open the GCP IAM permission reference for the service and find the
   IAM permission required for each API call (typically the page lists
   the permission alongside the API).
4. Map each API call into the right lifecycle stage:
   - **`plan`**: read calls Terraform issues during `plan` /
     refresh — usually `Get` and sometimes `List`.
   - **`create`**: calls Terraform issues for a new resource —
     typically `Insert` / `Create`.
   - **`update`**: calls Terraform issues for in-place updates —
     typically `Patch` / `Update`.
   - **`delete`**: calls Terraform issues during destroy — typically
     `Delete`.
5. Record the provider version and date in `verification`. Set
   `tested_against_provider` to the provider version range you assert
   the mapping holds for. If you only verified against `6.12.0` but
   believe the surface is stable across `>=5.0.0,<7.0.0`, that is a
   defensible range — the diagnostics command surfaces the raw range
   so users can compare it against their lockfile.

For a complete reference of an entry that exercises every schema
feature — including a `conditionals` block — see
`google_storage_bucket` in `catalog/storage.yaml`. The walkthrough
below shows the end-to-end docs+source workflow for a fresh service
that does not exist in the catalog yet.

### Worked example: `google_dataplex_lake`

This walkthrough adds Dataplex coverage from scratch using the
`docs+source` method. It illustrates the scaffold → map → validate
loop; the actual permission strings below are placeholders the
contributor verifies against the live docs and provider source on
their own machine.

**1. Scaffold the stub.** From the repository root:

```sh
tfperms catalog scaffold google_dataplex_lake
# created stub for google_dataplex_lake in catalog/dataplex.yaml
```

The command infers `dataplex.yaml` from the `google_dataplex_*`
prefix, creates the file (the service had no prior catalog
coverage), and writes one `resources:` entry with every required
schema field set to a `TODO` sentinel. The verb in the message is
`created` for a fresh file and `merged` when the scaffold appended
to an existing service file. The file parses but deliberately fails
`make catalog-validate` until the contributor fills the values in.

**2. Map permissions from the provider source.** Open the
[terraform-provider-google][1] file
`google/services/dataplex/resource_dataplex_lake.go` and enumerate
the distinct API calls:

| Lifecycle stage | Provider call (illustrative) | Required permission |
|----------------|------------------------------|---------------------|
| `plan`         | `Lakes.Get`                  | `dataplex.lakes.get` |
| `create`       | `Lakes.Create`               | `dataplex.lakes.create` |
| `update`       | `Lakes.Patch`                | `dataplex.lakes.update` |
| `delete`       | `Lakes.Delete`               | `dataplex.lakes.delete` |

Cross-reference each permission against the
[GCP Dataplex IAM permissions reference][3] to confirm the exact
string. Verify, do not guess — provider-side method names do not
always match the IAM permission verb (e.g. `Buckets.Insert` →
`storage.buckets.create`).

[3]: https://cloud.google.com/dataplex/docs/iam-permissions

**3. Fill in the entry.** Replace the TODO sentinels in
`catalog/dataplex.yaml` with the real values. The result looks like:

```yaml
resources:
  google_dataplex_lake:
    verification:
      method: docs+source
      source_urls:
        - https://cloud.google.com/dataplex/docs/iam-permissions
        - https://github.com/hashicorp/terraform-provider-google/blob/main/google/services/dataplex/resource_dataplex_lake.go
      verified_at: "2026-05-05"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan:
        - dataplex.lakes.get
      create:
        - dataplex.lakes.create
      update:
        - dataplex.lakes.update
      delete:
        - dataplex.lakes.delete
```

**4. Lock the mapping in the test.** Open
`internal/catalog/repo_test.go` and add expected `plan` / `create` /
`update` / `delete` lists for `google_dataplex_lake` to the
`TestRepositoryCatalogPermissionsAreLocked` table. Editing a
permission mapping is a deliberate change; the lock test is what
makes a future regression visible in code review.

**5. Validate.** Run:

```sh
make catalog-validate
```

The tests load every `catalog/*.yaml` file, check schema and
provenance, and verify the locked permissions match the YAML. A
clean exit means the entry is safe to PR.

If you later promote `google_dataplex_lake` into the empirical tier,
re-verify against a real GCP project, change `verification.method`
to `empirical`, refresh `verified_at`, and add the type to the
`topTierEmpiricalResources` map per the procedure above.

## Schema reference

```yaml
# resources/<terraform_type>: a `resource "<type>" "..."` block.
resources:
  google_<service>_<resource>:
    # REQUIRED. How the permission mapping was verified. method ∈
    # {empirical, docs+source}. source_urls cites the evidence.
    # verified_at is YYYY-MM-DD. verified_provider_version is the
    # exact provider release the verification ran against.
    verification:
      method: docs+source
      source_urls:
        - https://cloud.google.com/<service>/docs/access-control/iam-permissions
        - https://github.com/hashicorp/terraform-provider-google/blob/main/google/services/<service>/resource_<service>_<resource>.go
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"

    # REQUIRED. Provider version range the entry is asserted to hold
    # for. Free-form constraint expression — use the convention your
    # lockfile uses.
    tested_against_provider: ">=5.0.0,<7.0.0"

    # REQUIRED. Permissions split by Terraform lifecycle stage.
    #   plan:   read permissions used during `terraform plan` (must
    #           be non-empty).
    #   create: write permissions used to create a new instance.
    #   update: write permissions used to update an existing instance.
    #   delete: write permissions used to destroy an instance.
    # The other three stages are optional — an immutable resource
    # has no update permissions, for example.
    permissions:
      plan:
        - <service>.<resource>.<verb>
      create:
        - <service>.<resource>.<verb>
      update:
        - <service>.<resource>.<verb>
      delete:
        - <service>.<resource>.<verb>

    # OPTIONAL. Additive permissions that fire only when an attribute
    # on the underlying Terraform block matches a value. Each
    # conditional must have a non-empty `when` map and must contribute
    # at least one permission.
    conditionals:
      - when:
          <attribute>: <value>
        permissions:
          plan: [...]
          create: [...]
          update: [...]
          delete: [...]

# data_sources/<terraform_type>: a `data "<type>" "..."` block.
# Data sources are read-only — only `plan` permissions are accepted
# (the YAML schema rejects create / update / delete here).
data_sources:
  google_<service>_<resource>:
    verification:
      method: docs+source
      source_urls: [...]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [...]

# iam_bindings/<terraform_type>: an IAM binding/member/policy resource
# (e.g. google_storage_bucket_iam_binding). parent_resource MUST point
# at a Terraform resource type declared in the same merged catalog.
# Each lifecycle stage is filled in as if the binding were a
# first-class resource — in practice plan reads getIamPolicy and
# create / update / delete each call setIamPolicy.
iam_bindings:
  google_<service>_<resource>_iam_binding:
    parent_resource: google_<service>_<resource>
    verification:
      method: docs+source
      source_urls: [...]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [<resource>.getIamPolicy]
      create: [<resource>.setIamPolicy]
      update: [<resource>.setIamPolicy]
      delete: [<resource>.setIamPolicy]
```

## Style rules

- **Permissions are alphabetised** within each list. The diff is
  cleaner when adding new permissions; reviewers can skim instead of
  binary-searching.
- **Comments belong above non-obvious permissions** — when something
  is required for an unusual reason (a conditional, a side-effecting
  data source, an obscure verification path), a one-line comment
  saves the next contributor a Google search.
- **One service per file.** Resist the temptation to add
  `google_compute_*` to `storage.yaml` because it is convenient. A
  `compute.yaml` file is preferable even if it starts with one entry.
- **Resource names are exactly the Terraform type.** `google_storage_bucket`,
  not `gcs_bucket`. The analyzer matches by Terraform type string.

## Validation errors

When the validator rejects a file, the error is shaped like:

```
catalog: storage.yaml:42: resources/google_storage_bucket: permissions.plan must contain at least one permission
```

Read it left-to-right: file and line, then the entry path inside the
catalog, then the rule that was violated. The line number points at
the start of the entry that failed validation, not always at the
literal offending field — but the entry path narrows the search to a
few lines.

## Local feedback loop

```sh
make catalog-validate
```

This is the canonical local check for catalog changes. It wraps the
two test families that gate catalog correctness
(`TestCatalogValid*` and `TestRepositoryCatalog*`) and is the same
check CI runs. The full test suite is also fast (under a second):

```sh
go test ./internal/catalog/...
```

Together they cover:

- Schema unit tests for every individual rule (`TestValidate`).
- Loader merge / duplicate-detection / malformed-input coverage.
- A repository consistency test that loads the actual
  `catalog/*.yaml` and asserts every entry is valid.
- A provenance-completeness test that rejects stub `verified_at`
  values, off-floor dates, and missing source URLs.
- Per-resource permission-mapping tests
  (`TestRepositoryCatalogPermissionsAreLocked`) that lock the exact
  `plan` / `create` / `update` / `delete` lists for each catalog
  entry. **Editing a permission mapping is a deliberate change.**
  Update the expected values in `internal/catalog/repo_test.go`
  alongside the YAML so the test reviewer can see both sides of the
  change in the same diff.

Run it before opening a PR. If it passes locally it will pass in CI.

## Pull request checklist

Every catalog PR — whether it adds a single resource or a whole
service file — must satisfy this checklist before review:

- [ ] **`make catalog-validate` is clean** locally on the PR branch.
      No skipped tests, no `-run` filters that hide other failures.
- [ ] **Permissions are alphabetised** within each `plan` / `create` /
      `update` / `delete` list, and entries are ordered consistently
      within each section (alphabetical by Terraform type).
- [ ] **Provenance is complete and real.** Every entry carries
      `verification.method`, `source_urls`, `verified_at`, and
      `verified_provider_version`. `verified_at` is the date the
      verification ran (not the date you opened the PR), is on or
      after 2024-01-01, and points to authentic
      `cloud.google.com` or `github.com/hashicorp/terraform-provider-google`
      URLs.
- [ ] **Locked-permissions tests are updated** in
      `internal/catalog/repo_test.go` so any change to a
      `plan`/`create`/`update`/`delete` list is visible in the diff
      next to the YAML change.
- [ ] **Tier metadata is consistent.** If
      `verification.method` is `empirical`, the Terraform type also
      appears in `topTierEmpiricalResources` with a one-line
      rationale; if it is `docs+source`, it does not.
- [ ] **No stray TODO sentinels** from `tfperms catalog scaffold`
      remain in the YAML (the validator catches these, but call it
      out in the PR description if you intentionally split the work
      across multiple PRs).
