package awscreds

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	t.Run("session token without static credentials is an error", func(t *testing.T) {
		_, err := Credentials{SessionToken: "TOKEN", Region: "us-east-1"}.LoadConfig(ctx)
		if !errors.Is(err, ErrSessionTokenWithoutStaticCredentials) {
			t.Fatalf("err = %v, want ErrSessionTokenWithoutStaticCredentials", err)
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

	t.Run("default region is available to assume role provider", func(t *testing.T) {
		authorization := make(chan string, 1)
		stsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization <- r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(`<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <AssumedRoleUser>
      <Arn>arn:aws:sts::123456789012:assumed-role/ingestr/test</Arn>
      <AssumedRoleId>AROATEST:test</AssumedRoleId>
    </AssumedRoleUser>
    <Credentials>
      <AccessKeyId>ASSUME_ROLE_AKID</AccessKeyId>
      <SecretAccessKey>ASSUME_ROLE_SECRET</SecretAccessKey>
      <SessionToken>ASSUME_ROLE_SESSION_TOKEN</SessionToken>
      <Expiration>2100-01-01T00:00:00Z</Expiration>
    </Credentials>
  </AssumeRoleResult>
  <ResponseMetadata>
    <RequestId>request-id</RequestId>
  </ResponseMetadata>
</AssumeRoleResponse>`))
		}))
		defer stsServer.Close()

		configFile := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(configFile, []byte(`[profile role]
role_arn = arn:aws:iam::123456789012:role/ingestr
credential_source = Environment
`), 0o600); err != nil {
			t.Fatalf("write config file: %v", err)
		}

		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		t.Setenv("AWS_PROFILE", "")
		t.Setenv("AWS_CONFIG_FILE", configFile)
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "no-creds"))
		t.Setenv("AWS_ACCESS_KEY_ID", "SOURCE_AKID")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "SOURCE_SECRET")
		t.Setenv("AWS_SESSION_TOKEN", "")
		t.Setenv("AWS_ENDPOINT_URL_STS", stsServer.URL)

		cfg, err := Credentials{Profile: "role", DefaultRegion: "us-east-1"}.LoadConfig(ctx)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		creds, err := cfg.Credentials.Retrieve(ctx)
		if err != nil {
			t.Fatalf("retrieve assume role credentials: %v", err)
		}
		if creds.AccessKeyID != "ASSUME_ROLE_AKID" {
			t.Fatalf("access key id = %q, want ASSUME_ROLE_AKID", creds.AccessKeyID)
		}
		if got := <-authorization; !strings.Contains(got, "/us-east-1/sts/aws4_request") {
			t.Fatalf("authorization = %q, want us-east-1 signing region", got)
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
