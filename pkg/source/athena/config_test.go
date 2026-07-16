package athena

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAthenaConfig_NormalizesBucketToOutputLocation(t *testing.T) {
	cfg, err := parseAthenaConfig("athena://?bucket=my-bucket&access_key_id=ak&secret_access_key=sk&region_name=us-east-1")
	require.NoError(t, err)
	require.Equal(t, "s3://my-bucket/", cfg.OutputLocation)
}

func TestParseAthenaConfig_NormalizesS3URIToOutputLocation(t *testing.T) {
	cfg, err := parseAthenaConfig("athena://?bucket=s3://my-bucket/prefix&access_key_id=ak&secret_access_key=sk&region_name=us-east-1")
	require.NoError(t, err)
	require.Equal(t, "s3://my-bucket/prefix/", cfg.OutputLocation)
}

func TestParseAthenaConfig_RequiresBucket(t *testing.T) {
	_, err := parseAthenaConfig("athena://?access_key_id=ak&secret_access_key=sk&region_name=us-east-1")
	require.Error(t, err)
}

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
