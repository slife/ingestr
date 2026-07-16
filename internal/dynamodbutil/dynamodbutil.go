package dynamodbutil

import (
	"context"
	"fmt"
	"net/url"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/bruin-data/ingestr/internal/awscreds"
)

var awsEndpointPattern = regexp.MustCompile(`.*\.(.+)\.amazonaws\.com`)

type Config struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Profile         string
	EndpointURL     string
}

func ParseURI(uri string) (*Config, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("failed to parse dynamodb URI: %w", err)
	}

	query := u.Query()

	cfg := &Config{
		AccessKeyID:     query.Get("access_key_id"),
		SecretAccessKey: query.Get("secret_access_key"),
		SessionToken:    query.Get("session_token"),
		Profile:         query.Get("profile"),
	}

	if matches := awsEndpointPattern.FindStringSubmatch(u.Host); matches != nil {
		cfg.Region = matches[1]
		cfg.EndpointURL = fmt.Sprintf("https://%s", u.Hostname())
	} else if u.Host != "" {
		cfg.EndpointURL = fmt.Sprintf("http://%s", u.Host)
	}

	if cfg.Region == "" {
		cfg.Region = query.Get("region")
	}

	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, fmt.Errorf("both access_key_id and secret_access_key are required when using static credentials")
	}

	return cfg, nil
}

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
	if awsCfg.Region == "" {
		return nil, fmt.Errorf("region is required to connect to DynamoDB")
	}
	cfg.Region = awsCfg.Region

	client := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
		}
	})

	return client, nil
}
