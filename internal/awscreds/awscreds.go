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
// falling back to the AWS default credential chain. DefaultRegion is applied
// only after awsconfig.LoadDefaultConfig returns, so it never overrides a
// region resolved from the URI or the ambient environment/profile.
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
