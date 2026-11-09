// Package soltest starts a throwaway Solana test validator in Docker for
// tests. Import it only from _test.go files.
package soltest

import (
	"context"
	"fmt"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the validator image used in tests; it matches docker-compose.yml.
const Image = "anzaxyz/agave:v3.1.14"

// Validator is a running solana-test-validator.
type Validator struct {
	URL       string // JSON-RPC endpoint
	container *testcontainers.DockerContainer
}

// Start launches a fresh validator and waits until its RPC answers.
//
// Agave 3 needs io_uring, which Docker's default seccomp profile blocks,
// so the container runs without seccomp filtering. It is a local test
// container with one port mapped to the host's loopback.
func Start(ctx context.Context) (*Validator, error) {
	c, err := testcontainers.Run(ctx, Image,
		testcontainers.WithEntrypoint("solana-test-validator"),
		testcontainers.WithCmd("--ledger", "/tmp/ledger", "--reset", "--quiet"),
		testcontainers.WithExposedPorts("8899/tcp"),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.SecurityOpt = append(hc.SecurityOpt, "seccomp=unconfined")
		}),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("8899/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		return nil, fmt.Errorf("start solana validator: %w", err)
	}
	v := &Validator{container: c}
	ep, err := c.PortEndpoint(ctx, "8899/tcp", "http")
	if err != nil {
		v.Close()
		return nil, fmt.Errorf("validator endpoint: %w", err)
	}
	v.URL = ep
	if err := v.waitFinalized(ctx); err != nil {
		v.Close()
		return nil, err
	}
	return v, nil
}

// waitFinalized returns once the validator has finalized a slot, so
// finalized reads (which the watcher uses) work from the start.
func (v *Validator) waitFinalized(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := rpc.New(v.URL)
	for {
		if s, err := client.GetSlot(ctx, rpc.CommitmentFinalized); err == nil && s > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("validator never finalized a slot: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Close stops and removes the container.
func (v *Validator) Close() {
	if v.container != nil {
		_ = testcontainers.TerminateContainer(v.container)
	}
}
