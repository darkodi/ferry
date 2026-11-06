package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Message attribute names. The topic and id travel as attributes so the
// body stays the event's JSON payload unchanged.
const (
	attrTopic = "topic"
	attrID    = "message_id"
)

// SQS is a queue backed by Amazon SQS, or by ElasticMQ locally (same API).
type SQS struct {
	client *sqs.Client
	url    string
	// Wait is the long-poll time in seconds for Receive (0..20).
	Wait int32
}

// NewSQS connects to the queue at queueURL. Credentials and region come
// from the standard AWS environment (AWS_REGION, AWS_ACCESS_KEY_ID, …).
// endpoint overrides the SQS endpoint, e.g. http://localhost:9324 for
// ElasticMQ; leave it empty for real AWS.
func NewSQS(ctx context.Context, endpoint, queueURL string) (*SQS, error) {
	if queueURL == "" {
		return nil, errors.New("queue: SQS queue URL is required")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("queue: load AWS config: %w", err)
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	return &SQS{client: client, url: queueURL, Wait: 10}, nil
}

// Publish sends m.
func (q *SQS) Publish(ctx context.Context, m Message) error {
	_, err := q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.url),
		MessageBody: aws.String(string(m.Body)),
		MessageAttributes: map[string]types.MessageAttributeValue{
			attrTopic: {DataType: aws.String("String"), StringValue: aws.String(m.Topic)},
			attrID:    {DataType: aws.String("String"), StringValue: aws.String(m.ID)},
		},
	})
	if err != nil {
		return fmt.Errorf("queue: send %s: %w", m.ID, err)
	}
	return nil
}

// Receive long-polls for up to n messages (SQS caps a batch at 10).
func (q *SQS) Receive(ctx context.Context, n int) ([]Delivery, error) {
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(q.url),
		MaxNumberOfMessages:         int32(min(max(n, 1), 10)),
		WaitTimeSeconds:             q.Wait,
		MessageAttributeNames:       []string{attrTopic, attrID},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, fmt.Errorf("queue: receive: %w", err)
	}
	ds := make([]Delivery, 0, len(out.Messages))
	for _, m := range out.Messages {
		n, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
		ds = append(ds, Delivery{
			Message: Message{
				ID:    attr(m.MessageAttributes, attrID),
				Topic: attr(m.MessageAttributes, attrTopic),
				Body:  []byte(aws.ToString(m.Body)),
			},
			Receipt:  aws.ToString(m.ReceiptHandle),
			Receives: n,
		})
	}
	return ds, nil
}

// Ack deletes the delivered message.
func (q *SQS) Ack(ctx context.Context, d Delivery) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.url),
		ReceiptHandle: aws.String(d.Receipt),
	})
	if err != nil {
		return fmt.Errorf("queue: delete %s: %w", d.ID, err)
	}
	return nil
}

func attr(m map[string]types.MessageAttributeValue, k string) string {
	if v, ok := m[k]; ok {
		return aws.ToString(v.StringValue)
	}
	return ""
}
