package storage

import (
    "context"
    "encoding/json"
    "fmt"
    "time"
    "bytes"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3Real struct{
    bucket string
    prefix string
    cli *s3.Client
}

func NewS3Real(ctx context.Context, bucket, prefix string) (*s3Real, error) {
    cfg, err := config.LoadDefaultConfig(ctx) // IRSA-friendly in-cluster
    if err != nil { return nil, err }
    return &s3Real{bucket: bucket, prefix: prefix, cli: s3.NewFromConfig(cfg)}, nil
}

func (s *s3Real) Save(ctx context.Context, key string, rec *Record) (string, error) {
    // key under prefix
    k := key; if s.prefix != "" { k = s.prefix + "/" + key }
    b, err := json.MarshalIndent(rec, "", "  ")
    if err != nil { return "", err }
    _, err = s.cli.PutObject(ctx, &s3.PutObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(k + ".json"),
        Body:   bytes.NewReader(b),
        ContentType: aws.String("application/json"),
    })
    if err != nil { return "", err }
    return fmt.Sprintf("s3://%s/%s.json", s.bucket, k), nil
}
