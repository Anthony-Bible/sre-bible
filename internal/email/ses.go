package email

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// compile-time assertion.
var _ Transport = (*SESTransport)(nil)

// SESConfig holds static AWS credentials for SES.
type SESConfig struct {
	Region    string
	AccessKey string
	SecretKey string
}

// SESTransport delivers email via the AWS SES v2 API.
// Static credentials are used exclusively — the default credential chain
// (IMDS, Workload Identity) is never probed.
type SESTransport struct {
	client *sesv2.Client
}

// NewSESTransport creates an SESTransport with explicit static credentials.
func NewSESTransport(_ context.Context, cfg SESConfig) (*SESTransport, error) {
	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
	}
	client := sesv2.NewFromConfig(awsCfg)
	return &SESTransport{client: client}, nil
}

// Send delivers msg via SES simple email.
func (t *SESTransport) Send(ctx context.Context, msg Message) error {
	input := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(msg.From),
		Destination: &types.Destination{
			ToAddresses: []string{msg.To},
		},
		ReplyToAddresses: []string{msg.ReplyTo},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{
					Data:    aws.String(msg.Subject),
					Charset: aws.String("UTF-8"),
				},
				Body: &types.Body{
					Text: &types.Content{
						Data:    aws.String(msg.Body),
						Charset: aws.String("UTF-8"),
					},
				},
			},
		},
	}
	if _, err := t.client.SendEmail(ctx, input); err != nil {
		return fmt.Errorf("ses send email: %w", err)
	}
	return nil
}
