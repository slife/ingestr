package awscreds

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("static credentials are used when both keys present", func(t *testing.T) {
		t.Setenv("AWS_PROFILE", "")
		t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-creds"))
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
		t.Setenv("AWS_PROFILE", "")
		t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-creds"))
		cfg, err := Credentials{DefaultRegion: "us-east-1"}.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "us-east-1" {
			t.Fatalf("region = %q, want us-east-1 fallback", cfg.Region)
		}
	})

	t.Run("uri region wins over default region", func(t *testing.T) {
		t.Setenv("AWS_PROFILE", "")
		t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-creds"))
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
		t.Setenv("AWS_PROFILE", "")
		t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-creds"))
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
		t.Setenv("AWS_PROFILE", "")
		t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "no-config"))
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-creds"))
		cfg, err := Credentials{}.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "" {
			t.Fatalf("region = %q, want empty", cfg.Region)
		}
	})
}
