package queue

// Integration test of the SQS adapter against ElasticMQ in Docker, using
// the same config file as docker compose. Skipped with -short.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const elasticMQImage = "softwaremill/elasticmq-native:1.7.1" // matches docker-compose.yml

func startElasticMQ(t *testing.T) string {
	t.Helper()
	conf, err := filepath.Abs("../../../deploy/elasticmq.conf")
	if err != nil {
		t.Fatal(err)
	}
	c, err := testcontainers.Run(t.Context(), elasticMQImage,
		testcontainers.WithExposedPorts("9324/tcp"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath: conf, ContainerFilePath: "/opt/elasticmq.conf", FileMode: 0o644,
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/?Action=ListQueues").WithPort("9324/tcp")),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	endpoint, err := c.PortEndpoint(t.Context(), "9324/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "local")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local")
	return endpoint
}

func TestSQS(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	endpoint := startElasticMQ(t)
	ctx := t.Context()

	t.Run("publish receive ack", func(t *testing.T) {
		q, err := NewSQS(ctx, endpoint, endpoint+"/000000000000/ferry-payouts")
		if err != nil {
			t.Fatal(err)
		}
		q.Wait = 1
		want := Message{ID: "42", Topic: "payout.held", Body: []byte(`{"payout_id":"x"}`),
			TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
		if err := q.Publish(ctx, want); err != nil {
			t.Fatal(err)
		}
		if n, err := q.Depth(ctx, ""); err != nil || n != 1 {
			t.Fatalf("depth %d, %v; want 1", n, err)
		}
		got, err := q.Receive(ctx, 10)
		if err != nil || len(got) != 1 {
			t.Fatalf("receive: %v, %d messages", err, len(got))
		}
		d := got[0]
		if d.ID != want.ID || d.Topic != want.Topic || string(d.Body) != string(want.Body) || d.Receives != 1 || d.TraceParent != want.TraceParent {
			t.Fatalf("got %+v, want %+v with 1 receive", d, want)
		}
		if err := q.Ack(ctx, d); err != nil {
			t.Fatal(err)
		}
		if got, _ := q.Receive(ctx, 10); len(got) != 0 {
			t.Fatalf("acknowledged message came back: %+v", got)
		}
	})

	// Failure table: "Poison message" → after MaxReceives deliveries the
	// message moves to the DLQ instead of blocking the queue forever.
	t.Run("poison message goes to the DLQ", func(t *testing.T) {
		main, dlq := queuePair(ctx, t, endpoint)
		if err := main.Publish(ctx, Message{ID: "7", Topic: "payout.held", Body: []byte("not json")}); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= MaxReceives; i++ {
			got, err := main.Receive(ctx, 1)
			if err != nil || len(got) != 1 || got[0].Receives != i {
				t.Fatalf("delivery %d: %v %+v", i, err, got)
			}
			// not acknowledged: the consumer keeps failing
		}
		if got, _ := main.Receive(ctx, 1); len(got) != 0 {
			t.Fatalf("delivered a %d. time: %+v", MaxReceives+1, got)
		}
		got, err := dlq.Receive(ctx, 1)
		if err != nil || len(got) != 1 || got[0].ID != "7" {
			t.Fatalf("DLQ: %v %+v", err, got)
		}
	})
}

// queuePair creates a queue with a zero visibility timeout (so an
// unacknowledged message is redelivered at once) and its DLQ.
func queuePair(ctx context.Context, t *testing.T, endpoint string) (main, dlq *SQS) {
	t.Helper()
	q, err := NewSQS(ctx, endpoint, "unused")
	if err != nil {
		t.Fatal(err)
	}
	d, err := q.client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("test-dlq")})
	if err != nil {
		t.Fatal(err)
	}
	m, err := q.client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("test"),
		Attributes: map[string]string{
			string(types.QueueAttributeNameVisibilityTimeout): "0",
			string(types.QueueAttributeNameRedrivePolicy): fmt.Sprintf(
				`{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:test-dlq","maxReceiveCount":%d}`, MaxReceives),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &SQS{client: q.client, url: aws.ToString(m.QueueUrl)},
		&SQS{client: q.client, url: aws.ToString(d.QueueUrl)}
}
