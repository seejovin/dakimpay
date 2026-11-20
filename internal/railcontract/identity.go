package railcontract

import (
	"context"
	"errors"
)

// ProcessorIdentity identifies the durable financial ledger, independently of
// DNS, network location, software process, or credentials. A replacement ledger
// has a different identity, even if every other configuration field is identical.
type ProcessorIdentity interface {
	Identity(context.Context, Command) (string, error)
}

var ErrProcessorIdentityMismatch = errors.New("processor ledger identity mismatch")

type processorIdentityKey struct{}

// WithProcessorIdentity requires the connector and processor to bind the
// operation itself (including a negative lookup) to this exact ledger.
func WithProcessorIdentity(ctx context.Context, expected string) context.Context {
	return context.WithValue(ctx, processorIdentityKey{}, expected)
}

func ExpectedProcessorIdentity(ctx context.Context) string {
	id, _ := ctx.Value(processorIdentityKey{}).(string)
	return id
}
