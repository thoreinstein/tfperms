# Contributing to tfperms

This guide focuses on the area most contributions land in: the GCP
permission catalog at `catalog/*.yaml`. Code-level contributions follow
the conventions visible in `internal/parser/` and the existing tests;
the catalog has its own conventions and they are documented here.

## What the catalog is

`catalog/*.yaml` is the source of truth that maps Terraform resource
types onto the GCP IAM permissions tfperms reports as required for
plan and apply. Each YAML file is a partition by GCP service family
(`storage.yaml`, `compute.yaml`, ...). All files merge into a single
in-memory lookup at startup.

The schema and loader live in `internal/catalog/`. You almost never
need to read the Go code to add a catalog entry — the YAML is enough.

## Adding a new service file

1. Pick a service-level filename (e.g. `pubsub.yaml`).
2. Copy the structure from `catalog/storage.yaml`. It is intentionally
   the canonical example; every schema feature is exercised at least
   once.
3. Run the validator: `go test ./internal/catalog/...`. The tests
   include a repository consistency check that loads every
   `catalog/*.yaml` file and validates it. A new file with a missing
   field, an unknown verification method, or a dangling
   `parent_resource` will fail there with a `<file>:<line>` pointer
   to the offending node.

## Schema reference

```yaml
# resources/<terraform_type>: a `resource "<type>" "..."` block.
resources:
  google_<service>_<resource>:
    # REQUIRED. How an operator can verify the resource exists in GCP.
    # method ∈ {gcloud, rest, terraform}. command is a hint for humans
    # — tfperms never executes it.
    verification:
      method: gcloud
      command: "gcloud <service> describe ..."

    # REQUIRED. Permissions split by Terraform stage.
    #   plan:  required during `terraform plan` (must be non-empty).
    #   apply: required during `terraform apply` (may be empty for
    #          read-only entries; usually only data sources have an
    #          empty apply list).
    permissions:
      plan:
        - <service>.<resource>.<verb>
      apply:
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
          apply: [...]

# data_sources/<terraform_type>: a `data "<type>" "..."` block.
# Same shape as resources. The Apply list is usually empty.
data_sources:
  google_<service>_<resource>:
    verification:
      method: gcloud
    permissions:
      plan: [...]

# iam_bindings/<terraform_type>: an IAM binding/member/policy resource
# (e.g. google_storage_bucket_iam_binding). parent_resource MUST point
# at a Terraform resource type declared in the same merged catalog.
iam_bindings:
  google_<service>_<resource>_iam_binding:
    parent_resource: google_<service>_<resource>
    verification:
      method: rest
    permissions:
      plan: [<resource>.getIamPolicy]
      apply: [<resource>.setIamPolicy]
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
go test ./internal/catalog/...
```

The test suite is fast (under a second) and includes:

- Schema unit tests for every individual rule (`TestValidate`).
- Loader merge / duplicate-detection / malformed-input coverage.
- A repository consistency test that loads the actual
  `catalog/*.yaml` and asserts every entry is valid.

Run it before opening a PR. If it passes locally it will pass in CI.
