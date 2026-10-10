# Published schemas

The JSON Schemas here are byte-for-byte copies of the ones embedded in the
binaries (`pkg/gonkcfg`, `pkg/opercfg`); a test fails if a copy drifts, and a
recorded checksum makes every schema change deliberate.

| File | Describes | `$id` |
|---|---|---|
| `gonk-config.v1.schema.json` | a project's `.gonk.yml` | `https://raw.githubusercontent.com/gonk-factory/gonk/main/docs/schemas/gonk-config.v1.schema.json` |
| `gonk-operator.v1.schema.json` | the operator configuration | `https://raw.githubusercontent.com/gonk-factory/gonk/main/docs/schemas/gonk-operator.v1.schema.json` |

## Identity change, 2026-10-08

Both schemas' `$id` changed to the URLs above when the project's public home
became `github.com/gonk-factory/gonk`. Nothing else in either schema changed:
the same documents validate, and the schema version is still 1.

What a consumer can observe:

- A validation error cites the schema by `$id`, so the URL inside the error text
  gonk writes into a merge request or issue comment is the new one.
- The first comment line of the `.gonk.yml` gonk proposes in an onboarding merge
  request names the new home. A repository onboarded earlier keeps the line it
  has; it is a comment, and the file's meaning and its recorded content hash are
  unchanged for as long as the file is.
- A tool that pinned the old `$id` as an identifier must be updated.
