# tfperms

`tfperms` is a Go project building toward a static analyser that reads
a Terraform configuration targeting Google Cloud Platform and reports
the minimum IAM permissions required to run `terraform plan` and
`terraform apply` against it. The goal is to replace `roles/editor`-style
over-provisioning with a custom-role permission set the author can
audit and justify.

What ships today is the foundation that analyser will consume: a
curated catalog mapping Terraform GCP-provider resource, data-source,
and IAM-binding types onto IAM permissions, navigable via the
`tfperms catalog` subcommands (`scaffold` for stub entries, `stats` for
catalog health). The Terraform-configuration analyser that resolves a
real `*.tf` tree against this catalog from the root command is the
project's in-progress work, not a current feature.

The tool is a single Go binary. It does not contact GCP, does not need
credentials, and does not consume `terraform plan` JSON — the planned
analyser will parse the HCL on disk and resolve it against the embedded
permission catalog.

## What's in the box

- `cmd/tfperms` — the CLI entry point and `tfperms catalog ...`
  subcommands (`scaffold`, `stats`).
- `internal/parser` — HCL parsing and resolution.
- `internal/catalog` — schema, loader, validator, and stats for the
  YAML permission catalog.
- `catalog/*.yaml` — the permission catalog itself, partitioned by GCP
  service family. This is where most contributions land.
- `docs/tfperms_pdr.md` — product requirements doc; useful background
  if you are deciding whether a change is in or out of scope.

## Quick start

```sh
go install github.com/thoreinstein/tfperms/cmd/tfperms@latest
tfperms catalog stats                                # summarise the embedded catalog
tfperms catalog scaffold google_<service>_<resource> # write a stub entry under ./catalog/
```

`tfperms catalog --help` lists every supported subcommand. The bare
`tfperms` root command is reserved for the in-progress
Terraform-configuration analyser and is currently a no-op.

For local development:

```sh
make build           # build ./tfperms in the working directory
make test            # run the full test suite
make catalog-validate # assert the committed catalog satisfies all schema and provenance rules
make lint            # run golangci-lint with .golangci.yml
make help            # list every documented Make target
```

## Contributing

Most contributions add or refine entries in `catalog/*.yaml`. The
authoritative guide for those changes — schema reference, verification
tiers, the worked example, and the PR checklist — is in
[`CONTRIBUTING.md`](./CONTRIBUTING.md). Read it before opening a
catalog PR; the validator and review will reject entries that skip the
provenance and tier rules documented there.

Code-level contributions (parser, CLI, catalog tooling) follow the
conventions visible in the existing tests under `internal/`. Run
`make test lint catalog-validate` before opening a PR.

## License

See [`LICENSE`](./LICENSE).
