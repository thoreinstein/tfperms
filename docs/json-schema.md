# JSON Output Format

`tfperms` provides a stable, versioned JSON output format to enable programmatic consumption of analysis results. This is useful for automated policy enforcement, CI/CD gates, and custom reporting tools.

## Usage

To generate JSON output, use the `--format=json` flag:

```bash
tfperms --format=json [path]
```

## Schema Stability

The JSON output follows a versioned API contract. The current version is **1.0**, as indicated by the top-level `"version"` field.

### v1.x Stability Rules

*   **No Removal**: Existing fields will not be removed or renamed.
*   **No Type Changes**: The data type of existing fields will not change.
*   **Additive Changes**: New fields may be added in minor (v1.x) releases. Downstream consumers should be implemented to tolerate unknown fields.
*   **Deterministic Output**: JSON keys and array elements are sorted deterministically. Identical inputs will produce bit-identical JSON output, making it safe for `diff` usage in CI/CD pipelines.

## Schema Overview

The full JSON Schema is available at [`docs/schema/tfperms-output-v1.json`](schema/tfperms-output-v1.json).

### Top-Level Fields

*   `version`: The API schema version (currently "1.0").
*   `summary`: A high-level count of permissions, resources, and diagnostics.
*   `plan_permissions`: Permissions required for `terraform plan` (state refresh).
*   `apply_only_permissions`: Permissions required for `terraform apply` that are NOT in `plan_permissions`.
*   `total_apply_permissions`: The union of all permissions required for a full `terraform apply` cycle.
*   `resources`: Detailed attribution of permissions to specific Terraform resource blocks.
*   `diagnostics`: Parse-level warnings (e.g., non-local module sources).
*   `unknowns`: Terraform resources found in the configuration that are missing from the `tfperms` catalog.
*   `unresolved_conditionals`: Catalog-defined conditionals that could not be evaluated due to missing variables or dynamic values.
*   `metadata`: Information about the `tfperms` build and the generation timestamp.

## Resource Attribution

The `resources` array provides granular insight into why a permission was included. Each resource object contains a `permissions` array, which is the sorted, deduplicated union of all permissions required by that specific resource (including base permissions and any firing conditionals).

```json
{
  "type": "google_storage_bucket",
  "name": "data",
  "file": "main.tf",
  "line": 10,
  "permissions": [
    "storage.buckets.create",
    "storage.buckets.get"
  ]
}
```
