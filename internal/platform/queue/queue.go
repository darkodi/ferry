// Package queue moves events from the outbox relay to the workers.
//
// Delivery is at least once: a message can arrive twice (the relay died
// after publishing, or a worker died before acknowledging), so every
// consumer must be idempotent. A message that is not acknowledged comes
// back after the queue's visibility timeout, and after MaxReceives
// attempts it moves to a dead-letter queue for a human to inspect.
package queue

import "context"

// MaxReceives is how many times a message is delivered before it moves to
// the dead-letter queue. The SQS redrive policy (deploy/elasticmq.conf
// locally) uses the same number.
const MaxReceives = 5

// Message is one event. ID is the outbox row id, so a redelivered or
// republished event keeps the same ID and consumers can dedup on it.
type Message struct {
	ID    string
	Topic string
	Body  []byte
	// TraceParent is the W3C trace context of the request that caused the
	// message ("" for none); it travels as a message attribute.
	TraceParent string
}

// Delivery is a received message. Receipt identifies this delivery when
// acknowledging it; Receives counts deliveries of the message so far.
type Delivery struct {
	Message
	Receipt  string
	Receives int
}

// Publisher sends messages.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Consumer receives messages. Receive blocks up to the implementation's
// wait time and may return no messages. Ack deletes a handled message;
// a message that is never acknowledged is delivered again.
type Consumer interface {
	Receive(ctx context.Context, n int) ([]Delivery, error)
	Ack(ctx context.Context, d Delivery) error
}
