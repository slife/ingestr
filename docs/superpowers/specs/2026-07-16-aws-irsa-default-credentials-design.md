# AWS IRSA / Default-Credential-Chain Support — Design

**Date:** 2026-07-16
**Branch:** `slife/s3-irsa-support-vq6d7`
**Status:** Approved for implementation planning

## Problem

ingestr connects to AWS services (S3, Athena, DynamoDB, Kinesis, SQS, DuckLake-on-S3, Iceberg-on-S3/Glue) using `aws-sdk-go-v2`. Several of these paths **require** static `access_key_id`/`secret_access_key` in the connection URI and reject URIs that omit them. That makes ingestr unusable in environments that supply credentials ambiently — most importantly **AWS IRSA** (IAM Roles for Service Accounts on EKS, via `AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`), but also EC2 instance profiles, ECS task roles, env vars, and shared-config profiles.

The underlying SDK already supports all of these: `config.LoadDefaultConfig` resolves the full default credential chain, and it is already imported and called at 7 non-test sites. **No new dependencies are required.** The work is to (a) stop rejecting credential-less URIs, (b) resolve credentials through the default chain when explicit keys are absent, and (c) do this uniformly through one shared helper instead of the current per-connector divergence.

Secondary goals uncovered during survey:
- **Consistency.** Session-token handling, profile support, and region resolution differ across connectors today (e.g. blobstore hardcodes an empty session token and force-injects `us-east-1`, overriding the ambient `AWS_REGION`). Unifying them fixes latent bugs.
- **DuckLake** uses DuckDB SQL secrets (`CREATE SECRET`), not the AWS SDK, and hard-requires keys with `PROVIDER config`. It needs to emit `PROVIDER credential_chain` when keys are absent.
- **Documentation** across all AWS connectors overstates that keys are required and does not describe region resolution. It must be corrected consistently.

## Goals

1. Every AWS-SDK connector resolves credentials via the AWS default chain (env / shared config+profile / IRSA web-identity / ECS / EC2) when explicit keys are not supplied.
2. Static keys, `session_token`, and `profile` are supported uniformly wherever they make sense.
3. Region resolution is consistent and documented: URI → ambient (`AWS_REGION`/profile) → optional fallback.
4. DuckLake-on-S3 uses DuckDB's `credential_chain` provider when keys are omitted.
5. All AWS-connector documentation states the credential/region behavior concisely and consistently.

## Non-Goals

- **Redshift** is out of scope: it is a Postgres-wire connection (`redshift://user:password@host`, mapped to the postgres driver) and uses no AWS SDK credentials.
- **Explicit `role_arn` assume-role** parameters (à la the Kafka MSK-IAM path) are not added. The ambient default chain already resolves assumed roles via IRSA/instance profiles; explicit cross-account assume-role can be a later enhancement.
- **BigQuery `--staging-bucket` with `s3://`** is not made functional — that path currently rejects S3 (`parseGCSBucketURI` only accepts `gs://`). We only (optionally) clarify its misleading usage string.
- No change to non-AWS credential handling (GCS, Azure, SFTP).

## Architecture

### New unit: `internal/awscreds`

A single small package that owns the "credentials optional, otherwise fall back to the chain" contract.

```go
package awscreds

// Credentials is a normalized AWS credential set parsed from a connection URI.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	Profile         string

	// DefaultRegion is the last-resort region applied only when neither the URI
	// nor the ambient environment/profile resolves one. Empty means "no fallback":
	// callers that require an explicit region leave this empty and check
	// cfg.Region after loading.
	DefaultRegion string
}

// LoadConfig builds an aws.Config, preferring explicit input and otherwise
// falling back to the AWS default credential chain.
func (c Credentials) LoadConfig(ctx context.Context) (aws.Config, error)
```

**`LoadConfig` behavior:**

1. If exactly one of `AccessKeyID`/`SecretAccessKey` is set → return an error ("both access_key_id and secret_access_key are required when using static credentials"). This replaces the current silent-ignore behavior in blobstore.
2. If both keys are set → add `config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(AccessKeyID, SecretAccessKey, SessionToken))`. `SessionToken` may be empty.
3. If `Profile != ""` → add `config.WithSharedConfigProfile(Profile)`.
4. If `Region != ""` → add `config.WithRegion(Region)`.
5. Call `config.LoadDefaultConfig(ctx, opts...)`.
6. If the resolved `cfg.Region == ""` and `DefaultRegion != ""` → set `cfg.Region = DefaultRegion`. (This is why the fallback is applied *after* load: it must not override an ambient `AWS_REGION`.)
7. Return `cfg`. Callers that require a region (service connectors) check `cfg.Region == ""` themselves and error with a connector-specific message.

**Contract summary** — *What it does:* turns a normalized credential set into an `aws.Config` that prefers explicit input and otherwise uses the ambient chain, with region resolved URI → ambient → optional fallback. *How to use it:* parse the connector's existing URI params, populate `Credentials`, call `LoadConfig`, apply any connector-specific `BaseEndpoint`/path-style options to the resulting service client. *Depends on:* only `aws-sdk-go-v2/aws`, `/config`, `/credentials` — all already in `go.mod`.

**Note on parameter names:** the helper does **not** parse URIs. Each connector keeps its existing param names (`access_key_id` vs `aws_access_key_id`, `region` vs `region_name`, `storage_access_key`, etc.) and its existing endpoint/path-style handling; it only maps them into the `Credentials` struct. This preserves backward compatibility while centralizing the resolution logic.

### DuckLake exception

DuckLake does not use the AWS SDK; it configures S3 access through DuckDB SQL (`CREATE SECRET ... TYPE s3`). It cannot use `awscreds`. Instead:

- `ParseLakehouseURI` (`pkg/source/duckdb/lakehouse.go:110-137`): `storage_access_key`/`storage_secret_key` become **optional** for `storage_type=s3`. GCS is unchanged — it still requires `storage_access_key`/`storage_secret_key` because DuckDB's GCS secret uses HMAC keys with no credential-chain equivalent.
- `generateS3Secret` (`pkg/source/duckdb/lakehouse.go:272-304`): when both keys are present, emit `PROVIDER config` with `KEY_ID`/`SECRET` (unchanged). When keys are absent, emit `PROVIDER credential_chain` (omit `KEY_ID`/`SECRET`; keep `REGION`/`ENDPOINT`/`URL_STYLE`/`USE_SSL`/`SCOPE`). The early `return ""` when keys are missing must be removed so the chain secret is generated. DuckDB's `credential_chain` provider resolves env vars, shared config, IRSA/web-identity, and EC2/ECS roles.
- `storage_session_token` remains optional and, in `config` mode, continues to emit `SESSION_TOKEN`.

## Per-connector changes

| Connector | File / anchor | Change | Region policy (`DefaultRegion`) |
|-----------|---------------|--------|--------------------------------|
| Blobstore **source** | `pkg/source/blobstore/blobstore.go:202` (`loadAWSConfig`) | Rewrite onto `awscreds`. Add `session_token` + `profile` URI params to `parseS3BlobstoreURIOptions` + struct (session token is currently hardcoded `""` — a bug). | `us-east-1` |
| Blobstore **dest** | `pkg/destination/blobstore/blobstore.go:118` (`createS3Client`) | Rewrite onto `awscreds`, removing the inlined near-duplicate of the source logic. Add `session_token` + `profile`. | `us-east-1` |
| **Athena source** | `pkg/source/athena/config.go:37-39`, `pkg/source/athena/athena.go:48-70` | Remove the validation that rejects URIs with no profile/keys. Build the client via `awscreds`. Keep `bucket` required; keep the post-load "region required" check. | `""` (required) |
| **Athena dest** | `pkg/destination/athena/config.go:39-44`, `pkg/destination/athena/athena.go:88-110` | Same as Athena source. | `""` (required) |
| **DynamoDB** | `internal/dynamodbutil/dynamodbutil.go:51-56,61-80` | Make `access_key_id`/`secret_access_key` optional. Add `session_token` + `profile` params. Build client via `awscreds`. Keep host-based region derivation (`dynamodb.<region>.amazonaws.com`) and the `region` query param; keep "region required" error. Keep `BaseEndpoint` handling. | `""` (required) |
| **Kinesis** | `pkg/source/kinesis/kinesis.go:76-104,110-142` | Make `aws_access_key_id`/`aws_secret_access_key` optional. Add `profile` (session token already parsed). Build client via `awscreds`. Keep "region required" error and `BaseEndpoint` handling. | `""` (required) |
| **SQS** | `pkg/source/sqs/sqs.go:90-126,200-237` | Add `profile` param and route through `awscreds` (already keys-optional + session token + post-load region check). Behavior-preserving consolidation. | `""` (required) |
| **DuckLake** S3 | `pkg/source/duckdb/lakehouse.go:135-137,272-304` | See "DuckLake exception" above. GCS unchanged. | n/a (DuckDB `storage_region` default `us-east-1`, unchanged) |
| **Iceberg** dest | `pkg/destination/iceberg/config.go`, `iceberg.go:60` | **No code change** — already delegates to iceberg-go's default resolution when credential properties are absent. Add a confirming unit test. | n/a (iceberg-go / blobstore-class fallback) |

## Data flow (unchanged shape)

For SDK connectors: URI → connector-specific parse (existing param names) → `awscreds.Credentials{...}` → `LoadConfig(ctx)` → `aws.Config` → `service.NewFromConfig(cfg, connectorOpts...)` where `connectorOpts` still carries `BaseEndpoint`/`UsePathStyle`/path-style as today.

For DuckLake: URI → `ParseLakehouseURI` → `generateS3Secret` chooses `config` vs `credential_chain` → `CREATE SECRET` SQL executed by DuckDB.

## Backward compatibility

Two intentional behavior changes, both latent-bug fixes, both in the SDK connectors:

1. **Region fallback no longer overrides the environment.** Today blobstore force-injects `us-east-1` whenever the URI omits a region, overriding an ambient `AWS_REGION`. After: URI region → ambient `AWS_REGION`/profile → `us-east-1` last resort. Required for IRSA correctness outside us-east-1. Small risk: a user with `AWS_REGION` set who relied on the silent us-east-1 default now gets their real region.
2. **Both-or-neither key rule.** Providing exactly one of `access_key_id`/`secret_access_key` (a lone key) is an error. Providing neither now succeeds and resolves via the default chain; providing both uses static credentials as today. Blobstore previously *silently ignored* a lone key and fell through to the chain; this now errors — better typo detection.

### Where the both-or-neither (XOR) rule is enforced

- **`awscreds.LoadConfig`** is the single enforcement point and returns the lone-key error. This is the natural point for blobstore, whose parse step only extracts fields (there is no separately-validated blobstore config function).
- Connectors that have independently unit-tested parse/config functions (**Athena** `parseAthenaConfig`, **DynamoDB** `ParseURI`, **Kinesis** `parseKinesisURI`) additionally keep an early XOR check in that function, so the localized error and their existing table-driven tests stay meaningful. The change to those functions is narrow: replace "**both** keys required" with "**both-or-neither**" (region requirements unchanged). SQS already behaves this way.

### Existing tests to update (precise)

The current lone-key cases pass exactly one key, so they **continue to error** (reclassified from "missing key required" to "lone key not allowed") — they do **not** flip to success:
- `pkg/source/dynamodb/dynamodb_test.go:56-63` — "missing access_key_id" (lone secret) and "missing secret_access_key" (lone key) stay `wantErr: true`. "missing region" (`:66-68`) stays `wantErr: true`. Optionally rename to "lone secret_access_key" / "lone access_key_id" for clarity.
- `pkg/source/kinesis/kinesis_test.go:57-63` — same: both lone-key cases stay `wantErr: true`; "missing region" (`:66-68`) stays `wantErr: true`.

**New cases to add** (the actual IRSA path — neither key present):
- DynamoDB: `dynamodb://dynamodb.us-east-1.amazonaws.com` (no keys) → parses successfully (`wantErr: false`), keys empty.
- Kinesis: `kinesis://?region_name=us-east-1` (no keys) → parses successfully, keys empty.
- Athena: keyless config with `bucket` → no error.

All existing MinIO/static-key integration tests must remain green (they pass both keys, which still take the static-provider path).

## Testing

### Unit
- `internal/awscreds`: static-present vs. absent (chain path); session-token threading into the static provider; region matrix (URI region wins; ambient region preserved; `DefaultRegion` applied only when both empty; `DefaultRegion=""` leaves region empty); lone-key error; profile applied.
- Each connector's URI parsing: new params (`session_token`, `profile`) parsed; backward-compat aliases preserved; population into `Credentials` correct.
- DuckLake `generateS3Secret`: emits `PROVIDER credential_chain` (no `KEY_ID`/`SECRET`) when keys absent; `PROVIDER config` with `KEY_ID`/`SECRET`/optional `SESSION_TOKEN` when present; retains `REGION`/`ENDPOINT`/`URL_STYLE`/`USE_SSL`/`SCOPE`. `ParseLakehouseURI`: keyless `storage_type=s3` no longer errors; keyless `storage_type=gcs` still errors.
- Athena `parseAthenaConfig` (source + dest): keyless config no longer errors; `bucket` still required; lone-key still errors.
- DynamoDB/Kinesis: existing lone-key cases stay erroring; new no-keys cases parse successfully (see Backward compatibility for exact cases).
- Iceberg: confirming test that a keyless config produces properties without `s3.access-key-id`/`s3.secret-access-key` so iceberg-go falls back to its default chain.

### Integration
- Existing MinIO-based tests (static keys) must pass unchanged.
- True IRSA cannot run in CI (needs a real EKS pod with a web-identity token). The spec documents manual verification: deploy in an IRSA-enabled pod with a role granting the relevant permissions and no static keys in the URI, then run keyless ingests for `s3://`, `athena://`, `dynamodb://`, `kinesis://`, `sqs://`, `ducklake://?storage_type=s3`, and an Iceberg-on-S3 destination.

### Verification commands
`make format && make lint && make test`. No `go.mod`/`go.sum` change is expected (no new deps), so `make licenses-audit-update` should be a no-op; run it to confirm.

## Documentation

A single, consistent credential-resolution note is added to each AWS connector doc, with a per-connector region tail. Docs that currently claim keys are required are corrected.

### Reusable blurb (adjust param names per connector)

> **Credentials are optional.** When `access_key_id`/`secret_access_key` are omitted, ingestr resolves credentials through the standard AWS default credential chain: environment variables, a shared AWS config/credentials file (optionally selected with `profile`), EKS IRSA / web-identity roles, and ECS/EC2 instance-profile roles. You may also supply a `session_token` for temporary STS credentials. The region is taken from the URI first, then from the ambient environment (`AWS_REGION` or the selected profile); **[region tail]**.

Region tails:
- **Blobstore-class (S3, Iceberg):** "…if it still cannot be resolved, ingestr falls back to `us-east-1`."
- **Service connectors (Athena, DynamoDB, Kinesis, SQS):** "…if it still cannot be resolved, ingestr returns an error, since this connector requires an explicit region."

DuckLake variant (uses DuckDB's provider; `storage_*` names):
> **Credentials are optional.** When `storage_access_key`/`storage_secret_key` are omitted, DuckDB authenticates to S3 via its `credential_chain` provider, which uses the AWS default credential chain: environment variables, a shared AWS config/credentials file, EKS IRSA / web-identity roles, and ECS/EC2 instance-profile roles. `storage_session_token` may still be supplied for temporary STS credentials. `storage_region` defaults to `us-east-1` when unset.

### File-by-file (verified line anchors)

| File | Change |
|------|--------|
| `docs/supported-sources/s3.md` | Mark `access_key_id`/`secret_access_key` optional (L17-18); document region resolution (L19); replace "These credentials are required…" (L31) with the blurb; soften "you need an access_key_id and a secret_access_key" (L41); add one credential-less example. |
| `docs/supported-sources/athena.md` | Change keys from **(required)** to **(optional)** (L19); document region resolution + required-region (L21); add default-chain item after the credential-methods block (L29); soften the "Athena requires… access_key_id, secret_access_key" line (L33); add an IRSA note to the IAM-user step (L52-61). |
| `docs/supported-sources/dynamodb.md` | Mark keys optional + insert blurb (after L17); document newly-supported `session_token` and `profile`; document region derivation + required-region; soften the "AWS IAM access key pair" prerequisite (L24). |
| `docs/supported-sources/kinesis.md` | Mark `aws_access_key_id`/`aws_secret_access_key` optional + insert blurb (after L20, using `aws_*` names); document region resolution + required-region; document `profile`; soften "you need AWS credentials" (L26). |
| `docs/supported-sources/sqs.md` | Already documents the default chain — add the explicit term **IRSA**; document `profile`; align region wording with the blurb (L25-31). |
| `docs/supported-sources/iceberg.md` | Mark credential params optional + insert blurb (after L34); note `region` falls back to `us-east-1`; clarify the vague "region aliases" wording. |
| `docs/supported-sources/duckdb.md` | Add the DuckLake `credential_chain` note under `#### AWS S3` (after L115); flip `storage_access_key`/`storage_secret_key` from `yes` → `no` in the required-vs-optional table (L183-184) with the credential_chain note; leave the `storage_region` us-east-1 default as-is. |
| `docs/tutorials/load-kinesis-bigquery.md` | Reframe the "Required parameters" list (L58): keys optional, default chain / IRSA applies; `region_name` still required. Optionally soften Step 2 prose (L39). |
| `docs/supported-sources/kafka.md` | Already documents default chain + IRSA + EKS Pod Identity. No change required; optionally align wording. |
| `cmd/ingest.go` (L207) | Optional low-priority polish: note that `--staging-bucket` S3 credentials/region resolve via URI params or the default chain. |

No change needed: `README.md` (matrix only, no credential prose), `docs/supported-sources/platforms.md` (index), `docs/getting-started/data-masking.md` (incidental example).

## Open risks

- **DuckDB `credential_chain` availability.** Requires the `aws` + `httpfs` extensions, which `getRequiredExtensions` already loads for S3 (`lakehouse.go:225-226`). Verify the pinned DuckDB version accepts `PROVIDER credential_chain`.
- **Region fallback change (blobstore).** Documented above; acceptable and correct, but is a visible behavior change for anyone who set `AWS_REGION` and relied on the silent us-east-1 override.
