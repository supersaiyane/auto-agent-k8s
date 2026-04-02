package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3Real struct {
	bucket string
	prefix string
	cli    *s3.Client
}

func NewS3Real(ctx context.Context, bucket, prefix string) (*s3Real, error) {
	cfg, err := config.LoadDefaultConfig(ctx) // IRSA-friendly in-cluster
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}
	return &s3Real{bucket: bucket, prefix: prefix, cli: s3.NewFromConfig(cfg)}, nil
}

func (s *s3Real) Save(ctx context.Context, key string, rec *Record) (string, error) {
	k := key
	if s.prefix != "" {
		k = s.prefix + "/" + key
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("s3: marshal: %w", err)
	}
	_, err = s.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(k + ".json"),
		Body:        bytes.NewReader(b),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return "", fmt.Errorf("s3: put object: %w", err)
	}
	return fmt.Sprintf("s3://%s/%s.json", s.bucket, k), nil
}
