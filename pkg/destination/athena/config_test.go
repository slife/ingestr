package athena

import "testing"

func TestParseAthenaDestConfigKeylessAllowed(t *testing.T) {
	cfg, err := parseAthenaConfig("athena://mydb?bucket=my-bucket&region_name=us-east-1")
	if err != nil {
		t.Fatalf("keyless config should be allowed, got: %v", err)
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		t.Fatalf("expected empty creds")
	}
}
