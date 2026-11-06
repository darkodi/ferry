package queue

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

// Memory is an in-process queue for tests and the single-process dev
// mode. It behaves like SQS with a zero visibility timeout: a message
// that was received but not acknowledged is delivered again by the next
// Receive, and after MaxReceives deliveries it moves to Dead.
type Memory struct {
	mu      sync.Mutex
	msgs    []*memMsg
	dead    []Message
	receipt int
}

type memMsg struct {
	Message
	receives int
	receipt  string // of the latest delivery; "" once acknowledged
}

// NewMemory returns an empty queue.
func NewMemory() *Memory { return &Memory{} }

// Publish appends m to the queue.
func (q *Memory) Publish(ctx context.Context, m Message) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.msgs = append(q.msgs, &memMsg{Message: m})
	return nil
}

// Receive returns up to n messages, oldest first. It never blocks.
func (q *Memory) Receive(ctx context.Context, n int) ([]Delivery, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Delivery
	keep := q.msgs[:0]
	for _, m := range q.msgs {
		if len(out) == n {
			keep = append(keep, m)
			continue
		}
		if m.receives == MaxReceives {
			q.dead = append(q.dead, m.Message)
			continue
		}
		m.receives++
		q.receipt++
		m.receipt = strconv.Itoa(q.receipt)
		out = append(out, Delivery{Message: m.Message, Receipt: m.receipt, Receives: m.receives})
		keep = append(keep, m)
	}
	q.msgs = keep
	return out, nil
}

// Ack removes the delivered message. Acknowledging a stale delivery (the
// message was delivered again since) is a no-op, as in SQS.
func (q *Memory) Ack(ctx context.Context, d Delivery) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, m := range q.msgs {
		if m.receipt == d.Receipt {
			q.msgs = append(q.msgs[:i], q.msgs[i+1:]...)
			return nil
		}
	}
	return nil
}

// Len is the number of messages not yet acknowledged or dead-lettered.
func (q *Memory) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.msgs)
}

// Dead returns the messages moved to the dead-letter queue.
func (q *Memory) Dead() []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Message(nil), q.dead...)
}
