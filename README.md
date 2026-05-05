# tfperms

`tfperms` is a static analyser that reads a Terraform configuration
targeting Google Cloud Platform and reports the minimum IAM permissions
required to run `terraform plan` and `terraform apply` against it. The
goal is to replace `roles/editor`-style over-provisioning with a
custom-role permission set the author can audit and justify.

The tool is a single Go binary. It does not contact GCP, does not need
credentials, and does not consume `terraform plan` JSON — it parses the
HCL on disk and resolves it against an embedded permission catalog.

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
tfperms ./path/to/terraform/config
```

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
