package iceberg

import "testing"

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
