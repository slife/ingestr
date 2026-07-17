# AWS IRSA / Default-Credential-Chain Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let every AWS connector in ingestr resolve credentials via the AWS default credential chain (env / shared profile / IRSA web-identity / ECS / EC2) when explicit keys are omitted, through one shared helper.

**Architecture:** Add `internal/awscreds` with a `Credentials` struct and `LoadConfig` method that injects static credentials only when present, applies profile/region, calls `config.LoadDefaultConfig`, and applies a caller-chosen region fallback *after* load so it never overrides the environment. Route the SDK connectors (blobstore src/dst, Athena src/dst, DynamoDB, Kinesis, SQS) through it. DuckLake (DuckDB SQL secrets, not the SDK) gets `PROVIDER credential_chain` when keys are absent. Iceberg already delegates to iceberg-go's default resolution and needs only a confirming test.

**Tech Stack:** Go 1.26, `aws-sdk-go-v2` (`config`, `credentials`, `service/*`), DuckDB `aws`+`httpfs` extensions, apache/iceberg-go. No new dependencies.

## Global Constraints

- Module: `github.com/bruin-data/ingestr`; Go `1.26.5`.
- No new dependencies — `aws-sdk-go-v2/{aws,config,credentials}` are already in `go.mod`; `go.mod`/`go.sum` must not change.
- Do NOT add comments to self-explanatory code (project rule in CLAUDE.md).
- All Arrow timestamps are microseconds (not relevant here, but the convention stands).
- Preserve each connector's existing URI parameter names (`access_key_id` vs `aws_access_key_id`, `region` vs `region_name`, `storage_access_key`, etc.) and its existing endpoint / path-style handling. The helper does not parse URIs.
- Region policy per connector: **blobstore-class** (S3 blobstore src/dst, Iceberg) uses `DefaultRegion: "us-east-1"`; **service connectors** (Athena, DynamoDB, Kinesis, SQS) use `DefaultRegion: ""` and error if the region is unresolved.
- Both-or-neither key rule: exactly one of access-key/secret is an error; neither is valid (chain); both is static.
- Redshift is out of scope (Postgres-wire; no AWS SDK — verified).
- Run `make format && make lint && make test` before the final commit; expect `make licenses-audit-update` to be a no-op.

---

### Task 1: `internal/awscreds` shared helper

**Files:**
- Create: `internal/awscreds/awscreds.go`
- Test: `internal/awscreds/awscreds_test.go`

**Interfaces:**
- Consumes: `aws-sdk-go-v2/aws`, `/config`, `/credentials`.
- Produces:
  - `type Credentials struct { AccessKeyID, SecretAccessKey, SessionToken, Region, Profile, DefaultRegion string }`
  - `func (c Credentials) LoadConfig(ctx context.Context) (aws.Config, error)`
  - `var ErrIncompleteStaticCredentials = errors.New("both access_key_id and secret_access_key are required when using static credentials")`

- [ ] **Step 1: Write the failing test**

Create `internal/awscreds/awscreds_test.go`:

```go
package awscreds

import (
	"context"
	"errors"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("static credentials are used when both keys present", func(t *testing.T) {
		c := Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "TOKEN", Region: "eu-west-1"}
		cfg, err := c.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		creds, err := cfg.Credentials.Retrieve(ctx)
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		if creds.AccessKeyID != "AKID" || creds.SecretAccessKey != "SECRET" || creds.SessionToken != "TOKEN" {
			t.Fatalf("unexpected creds: %#v", creds)
		}
		if cfg.Region != "eu-west-1" {
			t.Fatalf("region = %q, want eu-west-1", cfg.Region)
		}
	})

	t.Run("lone access key is an error", func(t *testing.T) {
		_, err := Credentials{AccessKeyID: "AKID", Region: "us-east-1"}.LoadConfig(ctx)
		if !errors.Is(err, ErrIncompleteStaticCredentials) {
			t.Fatalf("err = %v, want ErrIncompleteStaticCredentials", err)
		}
	})

	t.Run("lone secret key is an error", func(t *testing.T) {
		_, err := Credentials{SecretAccessKey: "SECRET", Region: "us-east-1"}.LoadConfig(ctx)
		if !errors.Is(err, ErrIncompleteStaticCredentials) {
			t.Fatalf("err = %v, want ErrIncompleteStaticCredentials", err)
		}
	})

	t.Run("default region applied only when region otherwise empty", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		cfg, err := Credentials{DefaultRegion: "us-east-1"}.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "us-east-1" {
			t.Fatalf("region = %q, want us-east-1 fallback", cfg.Region)
		}
	})

	t.Run("uri region wins over default region", func(t *testing.T) {
		cfg, err := Credentials{Region: "ap-southeast-2", DefaultRegion: "us-east-1"}.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "ap-southeast-2" {
			t.Fatalf("region = %q, want ap-southeast-2", cfg.Region)
		}
	})

	t.Run("ambient region preserved when no default", func(t *testing.T) {
		t.Setenv("AWS_REGION", "ca-central-1")
		cfg, err := Credentials{}.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "ca-central-1" {
			t.Fatalf("region = %q, want ca-central-1 from env", cfg.Region)
		}
	})

	t.Run("empty default leaves region empty for required-region callers", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		cfg, err := Credentials{}.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "" {
			t.Fatalf("region = %q, want empty", cfg.Region)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/awscreds/...`
Expected: FAIL — `undefined: Credentials` / package has no non-test files.

- [ ] **Step 3: Write minimal implementation**

Create `internal/awscreds/awscreds.go`:

```go
package awscreds

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// ErrIncompleteStaticCredentials is returned when exactly one of the access key
// id / secret access key is supplied.
var ErrIncompleteStaticCredentials = errors.New("both access_key_id and secret_access_key are required when using static credentials")

// Credentials is a normalized AWS credential set parsed from a connection URI.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	Profile         string

	// DefaultRegion is applied only when neither the URI nor the ambient
	// environment/profile resolves a region. Empty means no fallback.
	DefaultRegion string
}

// LoadConfig builds an aws.Config, preferring explicit input and otherwise
// falling back to the AWS default credential chain.
func (c Credentials) LoadConfig(ctx context.Context) (aws.Config, error) {
	if (c.AccessKeyID == "") != (c.SecretAccessKey == "") {
		return aws.Config{}, ErrIncompleteStaticCredentials
	}

	var opts []func(*awsconfig.LoadOptions) error
	if c.AccessKeyID != "" && c.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken),
		))
	}
	if c.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, err
	}
	if cfg.Region == "" && c.DefaultRegion != "" {
		cfg.Region = c.DefaultRegion
	}
	return cfg, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/awscreds/...`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/awscreds/
git commit -m "feat(awscreds): shared AWS default-credential-chain helper"
```

---

### Task 2: Blobstore source on `awscreds`

**Files:**
- Modify: `pkg/source/blobstore/blobstore.go` (`parseS3BlobstoreURIOptions` ~1447-1452, `parsedBlobstoreURI` struct ~1373-1398, `loadAWSConfig` 202-220)
- Test: `pkg/source/blobstore/blobstore_test.go`

**Interfaces:**
- Consumes: `awscreds.Credentials`, `awscreds.LoadConfig` (Task 1).
- Produces: `parsedBlobstoreURI` gains `sessionToken` and `profile` string fields.

- [ ] **Step 1: Write the failing test**

Add to `pkg/source/blobstore/blobstore_test.go` (adjust the package clause if the existing test file uses `package blobstore`):

```go
func TestParseS3URICredentials(t *testing.T) {
	parsed, err := parseBlobstoreURI("s3://?access_key_id=AKID&secret_access_key=SECRET&session_token=TOK&region=eu-west-1&profile=prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.accessKeyID != "AKID" || parsed.secretAccessKey != "SECRET" {
		t.Fatalf("keys = %q/%q", parsed.accessKeyID, parsed.secretAccessKey)
	}
	if parsed.sessionToken != "TOK" {
		t.Fatalf("sessionToken = %q, want TOK", parsed.sessionToken)
	}
	if parsed.profile != "prod" {
		t.Fatalf("profile = %q, want prod", parsed.profile)
	}
	if parsed.region != "eu-west-1" {
		t.Fatalf("region = %q, want eu-west-1", parsed.region)
	}
}

func TestParseS3URINoCredentials(t *testing.T) {
	parsed, err := parseBlobstoreURI("s3://mybucket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.accessKeyID != "" || parsed.secretAccessKey != "" || parsed.sessionToken != "" {
		t.Fatalf("expected empty creds, got %#v", parsed)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/source/blobstore/ -run TestParseS3URI`
Expected: FAIL — `parsed.sessionToken`/`parsed.profile` undefined.

- [ ] **Step 3: Add struct fields**

In `pkg/source/blobstore/blobstore.go`, in `type parsedBlobstoreURI struct`, add after `secretAccessKey string`:

```go
	sessionToken    string
	profile         string
```

- [ ] **Step 4: Parse the new params**

In `parseS3BlobstoreURIOptions`, after the `parsed.secretAccessKey = q.Get("secret_access_key")` line, add:

```go
	parsed.sessionToken = q.Get("session_token")
	parsed.profile = q.Get("profile")
```

- [ ] **Step 5: Rewrite `loadAWSConfig` onto the helper**

Replace the entire `loadAWSConfig` function body. First add the import `"github.com/bruin-data/ingestr/internal/awscreds"` to the import block. Then:

```go
func loadAWSConfig(ctx context.Context, parsed *parsedBlobstoreURI, region string) (aws.Config, error) {
	if region == "" {
		region = parsed.region
	}
	return awscreds.Credentials{
		AccessKeyID:     parsed.accessKeyID,
		SecretAccessKey: parsed.secretAccessKey,
		SessionToken:    parsed.sessionToken,
		Region:          region,
		Profile:         parsed.profile,
		DefaultRegion:   "us-east-1",
	}.LoadConfig(ctx)
}
```

Remove the now-unused `credentials` and `awsconfig` imports **only if** they are unused elsewhere in the file (they are still used by `createS3Client`'s `awsconfig`? No — `createS3Client` uses `s3` and `aws`. Check: after this change `awsconfig` and `credentials` are unused in this file — remove both imports). Keep `aws` (used by `aws.String`).

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./pkg/source/blobstore/`
Expected: PASS (new tests + existing tests).

- [ ] **Step 7: Commit**

```bash
git add pkg/source/blobstore/
git commit -m "feat(blobstore-source): route S3 creds through awscreds, add session_token/profile"
```

---

### Task 3: Blobstore destination on `awscreds`

**Files:**
- Modify: `pkg/destination/blobstore/blobstore.go` (`createS3Client` 118-147, `parsedBlobstoreURI` struct 608-620, `parseBlobstoreURI` 631-636)
- Test: `pkg/destination/blobstore/blobstore_test.go`

**Interfaces:**
- Consumes: `awscreds.Credentials` (Task 1).
- Produces: destination `parsedBlobstoreURI` gains `sessionToken`/`profile`.

- [ ] **Step 1: Write the failing test**

Add to `pkg/destination/blobstore/blobstore_test.go`:

```go
func TestParseS3DestURICredentials(t *testing.T) {
	parsed, err := parseBlobstoreURI("s3://?access_key_id=AKID&secret_access_key=SECRET&session_token=TOK&region=eu-west-1&profile=prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.sessionToken != "TOK" || parsed.profile != "prod" {
		t.Fatalf("sessionToken/profile = %q/%q", parsed.sessionToken, parsed.profile)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/destination/blobstore/ -run TestParseS3DestURICredentials`
Expected: FAIL — `parsed.sessionToken`/`parsed.profile` undefined.

- [ ] **Step 3: Add struct fields**

In `type parsedBlobstoreURI struct`, add after `secretAccessKey string`:

```go
	sessionToken    string
	profile         string
```

- [ ] **Step 4: Parse the new params**

In `parseBlobstoreURI`, in the `case "s3":` block, after `parsed.secretAccessKey = u.Query().Get("secret_access_key")`, add:

```go
		parsed.sessionToken = u.Query().Get("session_token")
		parsed.profile = u.Query().Get("profile")
```

- [ ] **Step 5: Rewrite `createS3Client` onto the helper**

Add import `"github.com/bruin-data/ingestr/internal/awscreds"`. Replace the credential/config portion of `createS3Client` so the function reads:

```go
func createS3Client(ctx context.Context, parsed *parsedBlobstoreURI) (*s3.Client, error) {
	cfg, err := awscreds.Credentials{
		AccessKeyID:     parsed.accessKeyID,
		SecretAccessKey: parsed.secretAccessKey,
		SessionToken:    parsed.sessionToken,
		Region:          parsed.region,
		Profile:         parsed.profile,
		DefaultRegion:   "us-east-1",
	}.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}

	var s3Opts []func(*s3.Options)
	if parsed.endpointURL != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(parsed.endpointURL)
			o.UsePathStyle = true
		})
	}

	return s3.NewFromConfig(cfg, s3Opts...), nil
}
```

Remove the now-unused `awsconfig` and `credentials` imports from this file (verify they are not referenced elsewhere in the file first; `createGCSClient`/`createAzureClient` use neither).

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./pkg/destination/blobstore/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/destination/blobstore/
git commit -m "feat(blobstore-dest): route S3 creds through awscreds, add session_token/profile"
```

---

### Task 4: Athena source + destination (relax validation, use helper)

**Files:**
- Modify: `pkg/source/athena/config.go` (`parseAthenaConfig` 37-42), `pkg/source/athena/athena.go` (`Connect` 48-70)
- Modify: `pkg/destination/athena/config.go` (`parseAthenaConfig` 39-44), `pkg/destination/athena/athena.go` (`Connect` 88-110)
- Test: `pkg/source/athena/config_test.go`, `pkg/destination/athena/` (add config test if none)

**Interfaces:**
- Consumes: `awscreds.Credentials` (Task 1). Athena uses `DefaultRegion: ""` (region required post-load).
- Produces: no new exported symbols; `parseAthenaConfig` no longer errors on keyless URIs.

- [ ] **Step 1: Write the failing test (source)**

Add to `pkg/source/athena/config_test.go`:

```go
func TestParseAthenaConfigKeylessAllowed(t *testing.T) {
	cfg, err := parseAthenaConfig("athena://mydb?bucket=my-bucket&region_name=us-east-1")
	if err != nil {
		t.Fatalf("keyless config should be allowed, got: %v", err)
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		t.Fatalf("expected empty creds")
	}
}

func TestParseAthenaConfigLoneKeyRejected(t *testing.T) {
	_, err := parseAthenaConfig("athena://mydb?bucket=my-bucket&region_name=us-east-1&access_key_id=AKID")
	if err == nil {
		t.Fatal("lone access_key_id should be rejected")
	}
}

func TestParseAthenaConfigBucketRequired(t *testing.T) {
	_, err := parseAthenaConfig("athena://mydb?region_name=us-east-1")
	if err == nil {
		t.Fatal("missing bucket should be rejected")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/source/athena/ -run TestParseAthenaConfig`
Expected: FAIL — `TestParseAthenaConfigKeylessAllowed` errors because current validation rejects no-cred URIs.

- [ ] **Step 3: Relax validation (source config.go)**

In `pkg/source/athena/config.go`, delete this block:

```go
	if cfg.Profile == "" && cfg.AccessKeyID == "" && cfg.SecretAccessKey == "" && cfg.SessionToken == "" {
		return athenaConfig{}, errors.New("athena uri: provide either access_key_id/secret_access_key (optional session_token) or profile")
	}
```

Keep the both-or-neither check immediately below it:

```go
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return athenaConfig{}, errors.New("athena uri: both access_key_id and secret_access_key are required when using static credentials")
	}
```

- [ ] **Step 4: Route source Connect through helper**

In `pkg/source/athena/athena.go`, add import `"github.com/bruin-data/ingestr/internal/awscreds"`. Replace the credential/config-building block in `Connect` (the `loadOpts` construction + `LoadDefaultConfig` call) with:

```go
	awsCfg, err := awscreds.Credentials{
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		SessionToken:    cfg.SessionToken,
		Region:          cfg.Region,
		Profile:         cfg.Profile,
	}.LoadConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load aws config: %w", err)
	}
	if awsCfg.Region == "" {
		return errors.New("athena uri: region_name is required (or configure a default AWS region via profile/environment)")
	}
```

Remove now-unused imports `awsconfig` and `credentials` from `athena.go` if they are no longer referenced (verify: `athena.NewFromConfig`, `aws.String`, `types.*` remain; `awsconfig`/`credentials` become unused — remove them).

- [ ] **Step 5: Repeat for destination**

In `pkg/destination/athena/config.go`, delete the same "provide either… or profile" block (lines ~39-41), keeping the both-or-neither check.

In `pkg/destination/athena/athena.go`, add the `awscreds` import and replace the `loadOpts`/`LoadDefaultConfig` block in `Connect` with the same `awscreds.Credentials{...}.LoadConfig(ctx)` pattern (note: destination `athenaConfig` has the same field names — `AccessKeyID`, `SecretAccessKey`, `SessionToken`, `Region`, `Profile`). Keep the post-load `if awsCfg.Region == ""` error. Remove now-unused `awsconfig`/`credentials` imports.

Add to `pkg/destination/athena/config_test.go` (create if absent, `package athena`):

```go
func TestParseAthenaDestConfigKeylessAllowed(t *testing.T) {
	cfg, err := parseAthenaConfig("athena://mydb?bucket=my-bucket&region_name=us-east-1")
	if err != nil {
		t.Fatalf("keyless config should be allowed, got: %v", err)
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		t.Fatalf("expected empty creds")
	}
}
```

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./pkg/source/athena/ ./pkg/destination/athena/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/source/athena/ pkg/destination/athena/
git commit -m "feat(athena): allow keyless URIs (default chain), route through awscreds"
```

---

### Task 5: DynamoDB on `awscreds`

**Files:**
- Modify: `internal/dynamodbutil/dynamodbutil.go` (`Config` struct 17-22, `ParseURI` 51-56, `NewClient` 61-80)
- Test: `pkg/source/dynamodb/dynamodb_test.go` (existing `TestParseURI`)

**Interfaces:**
- Consumes: `awscreds.Credentials` (Task 1). DynamoDB uses `DefaultRegion: ""` (region required — checked in `ParseURI` already).
- Produces: `Config` gains `SessionToken` and `Profile` fields; keys become optional in `ParseURI`.

- [ ] **Step 1: Update the failing/changed tests**

In `pkg/source/dynamodb/dynamodb_test.go`, change the two lone-key cases to reflect the both-or-neither rule (they still error) and add a no-keys case. Rename for clarity and add:

```go
		{
			name:    "lone secret_access_key rejected",
			uri:     "dynamodb://dynamodb.us-east-1.amazonaws.com?secret_access_key=SECRET",
			wantErr: true,
		},
		{
			name:    "lone access_key_id rejected",
			uri:     "dynamodb://dynamodb.us-east-1.amazonaws.com?access_key_id=AKID",
			wantErr: true,
		},
		{
			name: "no keys resolves via default chain",
			uri:  "dynamodb://dynamodb.us-east-1.amazonaws.com",
			check: func(t *testing.T, cfg *dynamodbutil.Config) {
				if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
					t.Errorf("expected empty creds, got %q/%q", cfg.AccessKeyID, cfg.SecretAccessKey)
				}
				if cfg.Region != "us-east-1" {
					t.Errorf("Region = %q, want us-east-1", cfg.Region)
				}
			},
		},
```

(Delete the old "missing access_key_id"/"missing secret_access_key" entries these replace. If the existing table has no `check` field on error cases, keep the `wantErr` entries and add the no-keys case using whatever `check`/success mechanism the table already uses — match the existing struct shape.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/source/dynamodb/ -run TestParseURI`
Expected: FAIL — "no keys resolves via default chain" errors because `ParseURI` still requires keys.

- [ ] **Step 3: Add struct fields**

In `internal/dynamodbutil/dynamodbutil.go`, `type Config struct`, add:

```go
	SessionToken string
	Profile      string
```

- [ ] **Step 4: Parse new params, drop key requirement**

In `ParseURI`, set the new fields when building `cfg`:

```go
	cfg := &Config{
		AccessKeyID:     query.Get("access_key_id"),
		SecretAccessKey: query.Get("secret_access_key"),
		SessionToken:    query.Get("session_token"),
		Profile:         query.Get("profile"),
	}
```

Delete these two blocks (keep the region-required check):

```go
	if cfg.AccessKeyID == "" {
		return nil, fmt.Errorf("access_key_id is required to connect to DynamoDB")
	}
	if cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("secret_access_key is required to connect to DynamoDB")
	}
```

Add a both-or-neither check in their place:

```go
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, fmt.Errorf("both access_key_id and secret_access_key are required when using static credentials")
	}
```

- [ ] **Step 5: Route `NewClient` through the helper**

Replace `NewClient` credential loading. Add import `"github.com/bruin-data/ingestr/internal/awscreds"`. New body:

```go
func NewClient(ctx context.Context, cfg *Config) (*dynamodb.Client, error) {
	awsCfg, err := awscreds.Credentials{
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		SessionToken:    cfg.SessionToken,
		Region:          cfg.Region,
		Profile:         cfg.Profile,
	}.LoadConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
		}
	})

	return client, nil
}
```

Remove now-unused `awsconfig` and `credentials` imports (keep `aws` for `aws.String`).

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./pkg/source/dynamodb/ ./internal/dynamodbutil/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/dynamodbutil/ pkg/source/dynamodb/
git commit -m "feat(dynamodb): allow keyless URIs (default chain), add session_token/profile"
```

---

### Task 6: Kinesis on `awscreds`

**Files:**
- Modify: `pkg/source/kinesis/kinesis.go` (`kinesisCredentials` 38-44, `Connect` 76-104, `parseKinesisURI` 110-142)
- Test: `pkg/source/kinesis/kinesis_test.go` (existing `TestParseKinesisURI` / equivalent)

**Interfaces:**
- Consumes: `awscreds.Credentials` (Task 1). `DefaultRegion: ""` (region required — checked in `parseKinesisURI`).
- Produces: `kinesisCredentials` gains `Profile`; keys become optional.

- [ ] **Step 1: Update tests**

In `pkg/source/kinesis/kinesis_test.go`, change the lone-key cases to stay erroring (both-or-neither) and add a no-keys case:

```go
		{
			name:    "lone secret key rejected",
			uri:     "kinesis://?aws_secret_access_key=SECRET&region_name=us-east-1",
			wantErr: true,
		},
		{
			name:    "lone access key rejected",
			uri:     "kinesis://?aws_access_key_id=AKID&region_name=us-east-1",
			wantErr: true,
		},
		{
			name: "no keys resolves via default chain",
			uri:  "kinesis://?region_name=us-east-1",
			check: func(t *testing.T, c kinesisCredentials) {
				if c.AccessKeyID != "" || c.SecretAccessKey != "" {
					t.Errorf("expected empty creds, got %#v", c)
				}
				if c.Region != "us-east-1" {
					t.Errorf("Region = %q, want us-east-1", c.Region)
				}
			},
		},
```

(Delete the "missing access key"/"missing secret key" entries these replace. Match the existing table struct: if it lacks a `check` func, adapt to the existing success-assertion mechanism.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/source/kinesis/ -run TestParseKinesisURI`
Expected: FAIL — "no keys resolves via default chain" errors because `parseKinesisURI` still requires keys.

- [ ] **Step 3: Add Profile field**

In `type kinesisCredentials struct`, add:

```go
	Profile string
```

- [ ] **Step 4: Parse profile, drop key requirement**

In `parseKinesisURI`, add to the `creds` literal:

```go
		Profile: firstQuery(values, "aws_profile", "profile"),
```

Delete these two blocks (keep the region-required check):

```go
	if creds.AccessKeyID == "" {
		return kinesisCredentials{}, fmt.Errorf("kinesis URI: aws_access_key_id is required")
	}
	if creds.SecretAccessKey == "" {
		return kinesisCredentials{}, fmt.Errorf("kinesis URI: aws_secret_access_key is required")
	}
```

Add in their place:

```go
	if (creds.AccessKeyID == "") != (creds.SecretAccessKey == "") {
		return kinesisCredentials{}, fmt.Errorf("kinesis URI: both aws_access_key_id and aws_secret_access_key are required when using static credentials")
	}
```

- [ ] **Step 5: Route `Connect` through the helper**

Add import `"github.com/bruin-data/ingestr/internal/awscreds"`. Replace the `loadOpts`/`LoadDefaultConfig` block in `Connect` with:

```go
	awsCfg, err := awscreds.Credentials{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Region:          creds.Region,
		Profile:         creds.Profile,
	}.LoadConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
```

Remove now-unused `awsconfig` and `credentials` imports (keep `aws`).

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./pkg/source/kinesis/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/source/kinesis/
git commit -m "feat(kinesis): allow keyless URIs (default chain), add profile"
```

---

### Task 7: SQS on `awscreds` (add profile, consolidate)

**Files:**
- Modify: `pkg/source/sqs/sqs.go` (`sqsConfig` 39-47, `Connect` 90-126, `parseSQSURI` 200-237)
- Test: `pkg/source/sqs/sqs_test.go`

**Interfaces:**
- Consumes: `awscreds.Credentials` (Task 1). `DefaultRegion: ""` (region required — checked post-load in `Connect`).
- Produces: `sqsConfig` gains `Profile`.

- [ ] **Step 1: Write the failing test**

Add to `pkg/source/sqs/sqs_test.go`:

```go
func TestParseSQSURIProfile(t *testing.T) {
	cfg, err := parseSQSURI("sqs://?region=us-east-1&profile=prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Profile != "prod" {
		t.Fatalf("Profile = %q, want prod", cfg.Profile)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/source/sqs/ -run TestParseSQSURIProfile`
Expected: FAIL — `cfg.Profile` undefined.

- [ ] **Step 3: Add Profile field**

In `type sqsConfig struct`, add:

```go
	Profile string
```

- [ ] **Step 4: Parse profile**

In `parseSQSURI`, add to the `cfg` literal:

```go
		Profile: firstQuery(q, "profile", "aws_profile"),
```

- [ ] **Step 5: Route `Connect` through the helper**

Add import `"github.com/bruin-data/ingestr/internal/awscreds"`. Replace the `loadOpts`/lone-key-check/`LoadDefaultConfig` block in `Connect` with:

```go
	awsCfg, err := awscreds.Credentials{
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		SessionToken:    cfg.SessionToken,
		Region:          cfg.Region,
		Profile:         cfg.Profile,
	}.LoadConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	if awsCfg.Region == "" {
		return fmt.Errorf("sqs URI: region is required")
	}
```

Remove now-unused `awsconfig` and `credentials` imports (keep `aws`).

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./pkg/source/sqs/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/source/sqs/
git commit -m "feat(sqs): add profile support, consolidate onto awscreds"
```

---

### Task 8: DuckLake — `PROVIDER credential_chain` when keys absent

**Files:**
- Modify: `pkg/source/duckdb/lakehouse.go` (`ParseLakehouseURI` 135-137, `generateS3Secret` 272-304)
- Test: `pkg/source/duckdb/lakehouse_test.go`

**Interfaces:**
- Consumes: nothing new (DuckDB SQL, not the AWS SDK).
- Produces: keyless `storage_type=s3` no longer errors; `generateS3Secret` emits `PROVIDER credential_chain` when keys absent. GCS unchanged.

- [ ] **Step 1: Write the failing tests**

Add to `pkg/source/duckdb/lakehouse_test.go`:

```go
func TestParseLakehouseURIS3KeylessAllowed(t *testing.T) {
	cfg, err := ParseLakehouseURI("ducklake://?catalog_type=sqlite&catalog_path=/tmp/c.db&storage_type=s3&storage_path=s3://bucket/data")
	if err != nil {
		t.Fatalf("keyless S3 storage should be allowed, got: %v", err)
	}
	if cfg.Storage.AccessKey != "" || cfg.Storage.SecretKey != "" {
		t.Fatalf("expected empty storage keys")
	}
}

func TestParseLakehouseURIGCSStillRequiresKeys(t *testing.T) {
	_, err := ParseLakehouseURI("ducklake://?catalog_type=sqlite&catalog_path=/tmp/c.db&storage_type=gcs&storage_path=gs://bucket/data")
	if err == nil {
		t.Fatal("keyless GCS storage should still be rejected")
	}
}

func TestGenerateS3SecretCredentialChainWhenKeyless(t *testing.T) {
	l := NewLakehouseAttacher()
	sql := l.generateS3Secret("ingestr_storage", StorageConfig{Type: StorageTypeS3, Path: "s3://bucket/data", Region: "us-east-1"})
	if !strings.Contains(sql, "PROVIDER credential_chain") {
		t.Fatalf("expected PROVIDER credential_chain, got:\n%s", sql)
	}
	// Note: the header line "CREATE OR REPLACE SECRET <name> (" contains the
	// substring "SECRET " (trailing space), so match "SECRET '" (trailing
	// quote) to detect only a leaked `,   SECRET 'value'` config line.
	if strings.Contains(sql, "KEY_ID") || strings.Contains(sql, "SECRET '") {
		t.Fatalf("credential_chain secret must not embed keys, got:\n%s", sql)
	}
	if !strings.Contains(sql, "REGION 'us-east-1'") {
		t.Fatalf("expected REGION preserved, got:\n%s", sql)
	}
}

func TestGenerateS3SecretConfigWhenKeysPresent(t *testing.T) {
	l := NewLakehouseAttacher()
	sql := l.generateS3Secret("ingestr_storage", StorageConfig{Type: StorageTypeS3, Path: "s3://bucket/data", AccessKey: "AKID", SecretKey: "SECRET"})
	if !strings.Contains(sql, "PROVIDER config") {
		t.Fatalf("expected PROVIDER config, got:\n%s", sql)
	}
	if !strings.Contains(sql, "KEY_ID 'AKID'") || !strings.Contains(sql, "SECRET 'SECRET'") {
		t.Fatalf("expected embedded keys, got:\n%s", sql)
	}
}
```

Ensure `"strings"` is imported in the test file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/source/duckdb/ -run 'TestParseLakehouseURIS3Keyless|TestGenerateS3Secret'`
Expected: FAIL — `ParseLakehouseURI` errors on keyless S3, and `generateS3Secret` returns `""` for keyless.

- [ ] **Step 3: Make S3 keys optional in `ParseLakehouseURI`**

Replace the mandatory-key check:

```go
	if cfg.Storage.AccessKey == "" || cfg.Storage.SecretKey == "" {
		return nil, fmt.Errorf("storage_access_key and storage_secret_key are required for storage_type=%s", cfg.Storage.Type)
	}
```

with a rule that keeps GCS strict but allows keyless S3, plus a both-or-neither check for S3:

```go
	switch cfg.Storage.Type {
	case StorageTypeGCS:
		if cfg.Storage.AccessKey == "" || cfg.Storage.SecretKey == "" {
			return nil, fmt.Errorf("storage_access_key and storage_secret_key are required for storage_type=gcs")
		}
	case StorageTypeS3:
		if (cfg.Storage.AccessKey == "") != (cfg.Storage.SecretKey == "") {
			return nil, fmt.Errorf("storage_access_key and storage_secret_key must both be set (or both omitted to use the AWS credential chain) for storage_type=s3")
		}
	}
```

- [ ] **Step 4: Emit `credential_chain` when keys absent in `generateS3Secret`**

Replace the current `generateS3Secret` so it branches on key presence. The early `return ""` must go:

```go
func (l *LakehouseAttacher) generateS3Secret(name string, st StorageConfig) string {
	useChain := st.AccessKey == "" || st.SecretKey == ""

	parts := []string{
		"CREATE OR REPLACE SECRET " + name + " (",
		"    TYPE s3",
	}
	if useChain {
		parts = append(parts, ",   PROVIDER credential_chain")
	} else {
		parts = append(parts,
			",   PROVIDER config",
			",   KEY_ID "+quoteSQLStringLiteral(st.AccessKey),
			",   SECRET "+quoteSQLStringLiteral(st.SecretKey),
		)
		if st.SessionToken != "" {
			parts = append(parts, ",   SESSION_TOKEN "+quoteSQLStringLiteral(st.SessionToken))
		}
	}
	if st.Region != "" {
		parts = append(parts, ",   REGION "+quoteSQLStringLiteral(st.Region))
	}
	if st.Endpoint != "" {
		parts = append(parts, ",   ENDPOINT "+quoteSQLStringLiteral(st.Endpoint))
	}
	if st.URLStyle != "" {
		parts = append(parts, ",   URL_STYLE "+quoteSQLStringLiteral(st.URLStyle))
	}
	if st.UseSSL != nil {
		parts = append(parts, ",   USE_SSL "+strconv.FormatBool(*st.UseSSL))
	}
	scope := st.Path
	if scope == "" {
		scope = "s3://"
	}
	parts = append(parts, ",   SCOPE "+quoteSQLStringLiteral(scope), ")")
	return strings.Join(parts, "\n")
}
```

- [ ] **Step 5: Run tests to verify pass**

Run: `go test ./pkg/source/duckdb/`
Expected: PASS.

Note the `TestGenerateS3SecretCredentialChainWhenKeyless` assertion checks `SECRET '` (trailing quote), NOT `SECRET ` (trailing space): the always-present header line `CREATE OR REPLACE SECRET <name> (` contains `SECRET ` with a space, so a space-only match would false-positive on every secret. Only the `config` branch's `,   SECRET 'value'` line contains `SECRET '`.

- [ ] **Step 6: Commit**

```bash
git add pkg/source/duckdb/
git commit -m "feat(ducklake): use DuckDB credential_chain for S3 when keys omitted"
```

---

### Task 9: Iceberg confirming test (no code change)

**Files:**
- Test: `pkg/destination/iceberg/config_test.go` (add to existing)

**Interfaces:**
- Consumes: existing `parseIcebergConfig` / `applyPropertyAliases`.
- Produces: a test proving keyless configs omit `s3.access-key-id`/`s3.secret-access-key` so iceberg-go falls back to its default chain.

- [ ] **Step 1: Write the test**

Add to `pkg/destination/iceberg/config_test.go`:

```go
func TestIcebergKeylessConfigOmitsS3Credentials(t *testing.T) {
	cfg, err := parseIcebergConfig("iceberg+glue://?warehouse_bucket=my-bucket&region=us-east-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cfg.Properties["s3.access-key-id"]; ok {
		t.Fatal("keyless config must not set s3.access-key-id (would block default chain)")
	}
	if _, ok := cfg.Properties["s3.secret-access-key"]; ok {
		t.Fatal("keyless config must not set s3.secret-access-key")
	}
}
```

- [ ] **Step 2: Run test to verify it passes immediately**

Run: `go test ./pkg/destination/iceberg/ -run TestIcebergKeylessConfigOmitsS3Credentials`
Expected: PASS (no production change needed — this locks in the existing behavior).

If it FAILS (e.g. the exact URI shape doesn't parse), adjust the URI to a valid keyless iceberg config that this package accepts (consult neighboring cases in `config_test.go`), keeping the two "must not be set" assertions.

- [ ] **Step 3: Commit**

```bash
git add pkg/destination/iceberg/
git commit -m "test(iceberg): confirm keyless config defers S3 auth to default chain"
```

---

### Task 10: Documentation — consistent credential-resolution notes

**Files:**
- Modify: `docs/supported-sources/s3.md`, `athena.md`, `dynamodb.md`, `kinesis.md`, `sqs.md`, `iceberg.md`, `duckdb.md`
- Modify: `docs/tutorials/load-kinesis-bigquery.md`

**Interfaces:** none (docs only).

**Reusable blurb** (adjust key/param names per connector). Region tail = "…if it still cannot be resolved, ingestr falls back to `us-east-1`." for **S3, Iceberg**; "…if it still cannot be resolved, ingestr returns an error, since this connector requires an explicit region." for **Athena, DynamoDB, Kinesis, SQS**:

> **Credentials are optional.** When `access_key_id`/`secret_access_key` are omitted, ingestr resolves credentials through the standard AWS default credential chain: environment variables, a shared AWS config/credentials file (optionally selected with `profile`), EKS IRSA / web-identity roles, and ECS/EC2 instance-profile roles. You may also supply a `session_token` for temporary STS credentials. The region is taken from the URI first, then from the ambient environment (`AWS_REGION` or the selected profile); [region tail].

- [ ] **Step 1: s3.md**

In `docs/supported-sources/s3.md`: mark `access_key_id`/`secret_access_key` **(optional)** in the URI-parameter list; add `session_token` and `profile` to that list; replace the "These credentials are required to authenticate…" sentence with the blurb (S3 region tail); soften "you need an `access_key_id` and a `secret_access_key`" in the "Setting up an S3 Integration" section to "you can provide an `access_key_id`/`secret_access_key`, or rely on the AWS default credential chain (see URI Format)"; add one credential-less example: `s3://my-bucket` captioned "using the default credential chain / IRSA".

- [ ] **Step 2: athena.md**

Change `access_key_id`/`secret_access_key` from **(required)** to **(optional)** in the URI-parameter list; document that region is resolved URI → ambient and is **required** (no fallback); add the blurb (Athena region tail) after the credential-methods block; soften the "Athena requires a `bucket`, `access_key_id`, `secret_access_key` and `region_name`" sentence to "Athena requires a `bucket` and a resolvable region; credentials are optional"; add a note to the IAM-user step that on EKS/EC2 you can skip static keys and attach policies to the pod/instance role (IRSA).

- [ ] **Step 3: dynamodb.md**

Mark `access_key_id`/`secret_access_key` optional; document the newly supported `session_token` and `profile`; add the blurb (DynamoDB region tail), noting region derives from the URI host `dynamodb.<region>.amazonaws.com` or the `region` param, then ambient, and is required; soften the "AWS IAM access key pair" prerequisite to "AWS credentials via access key pair, or an ambient role (env / profile / IRSA / instance profile)".

- [ ] **Step 4: kinesis.md**

Mark `aws_access_key_id`/`aws_secret_access_key` optional; document `profile` (`aws_profile`); add the blurb using the `aws_*` names (Kinesis region tail — required); soften "you need AWS credentials with appropriate permissions" to present the default chain / IRSA as an alternative to static keys.

- [ ] **Step 5: sqs.md**

In the Authentication section (which already describes the default chain), add the explicit term **IRSA** alongside "web identity credentials … EKS"; document `profile`; align region wording with the blurb (region resolved URI → ambient; required).

- [ ] **Step 6: iceberg.md**

Mark the S3/Glue credential params optional; add the blurb (Iceberg = S3 region tail, `us-east-1` fallback); clarify the vague "region aliases" wording by enumerating the accepted forms (`region`/`region_name`).

- [ ] **Step 7: duckdb.md**

Under `#### AWS S3`, add: "When `storage_access_key`/`storage_secret_key` are omitted, DuckDB authenticates to S3 via its `credential_chain` provider, which uses the AWS default credential chain: environment variables, a shared AWS config/credentials file, EKS IRSA / web-identity roles, and ECS/EC2 instance-profile roles. `storage_session_token` may still be supplied for temporary STS credentials. `storage_region` defaults to `us-east-1` when unset." In the required-vs-optional parameter table, change `storage_access_key` and `storage_secret_key` from `yes` to `no` with note "falls back to DuckDB credential_chain (env / profile / IRSA / instance role)".

- [ ] **Step 8: load-kinesis-bigquery.md**

Reframe the "Required parameters" list for the Kinesis `--source-uri`: note that `aws_access_key_id`/`aws_secret_access_key` are optional and the default chain / IRSA applies when omitted; `region_name` remains required.

- [ ] **Step 9: Commit**

```bash
git add docs/
git commit -m "docs: document AWS default-credential-chain / IRSA behavior across connectors"
```

---

### Task 11: Final verification

**Files:** none (verification only).

- [ ] **Step 1: Format**

Run: `make format`
Expected: exits 0; commit any formatting changes it produces.

- [ ] **Step 2: Lint**

Run: `make lint`
Expected: no findings. Fix any (commonly: leftover unused `awsconfig`/`credentials` imports in the connectors touched in Tasks 2-7).

- [ ] **Step 3: Unit tests**

Run: `make test`
Expected: PASS with race detector.

- [ ] **Step 4: Confirm no dependency drift**

Run: `git diff --stat go.mod go.sum`
Expected: no output (unchanged). If changed, run `make licenses-audit-update` and investigate — the design adds no dependencies.

- [ ] **Step 5: Final commit (only if format/lint produced changes)**

```bash
git add -A
git commit -m "chore: format and lint AWS IRSA credential support"
```

---

## Self-Review

**Spec coverage:**
- `internal/awscreds` helper → Task 1. ✓
- Blobstore src/dst → Tasks 2, 3. ✓ (session_token + profile + region-fallback fix via `DefaultRegion`)
- Athena src/dst validation relax → Task 4. ✓
- DynamoDB → Task 5. ✓ (session_token + profile + keyless)
- Kinesis → Task 6. ✓ (profile + keyless)
- SQS → Task 7. ✓ (profile + consolidation)
- DuckLake credential_chain → Task 8. ✓ (GCS unchanged)
- Iceberg confirming test → Task 9. ✓
- Documentation pass → Task 10. ✓
- Backward-compat test flips (dynamodb/kinesis lone-key stay error; add no-key success) → Tasks 5, 6. ✓
- Region policy split (blobstore-class fallback vs service required) → encoded via `DefaultRegion` in each task. ✓
- Redshift out of scope → noted in Global Constraints. ✓
- `make format/lint/test`, no dep drift → Task 11. ✓

**Placeholder scan:** No "TBD"/"handle appropriately"/"similar to Task N" — each code step shows real code. The `[region tail]` in Task 10 is a documented template with both variants spelled out. ✓

**Type consistency:** `awscreds.Credentials` field names (`AccessKeyID`, `SecretAccessKey`, `SessionToken`, `Region`, `Profile`, `DefaultRegion`) and method `LoadConfig(ctx)` are used identically in Tasks 2-7. `ErrIncompleteStaticCredentials` referenced only in Task 1's own tests. DuckLake `generateS3Secret(name string, st StorageConfig)` signature preserved in Task 8. ✓
