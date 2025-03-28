// Package providing implements an exchange wrapper which
// does content providing for new blocks.
package providing

import (
	"context"
)

// Option defines the functional option type that can be used to configure
// providing Exchange instances
type Option func(*Exchange)

// WithProvideEnabled allows to enable or disable providing of blocks.
// When disabled, the Exchange will not announce blocks to the network.
// This is useful to avoid duplicate provides when blocks are added through
// multiple paths.
func WithProvideEnabled(enabled bool) Option {
	return func(ex *Exchange) {
		ex.provideEnabled = enabled
	}
}

// WithContextProvideKey is a key for context values that can be used to
// suppress providing for a specific call to NotifyNewBlocks.
type WithContextProvideKey struct{}

// ContextWithSuppressProvide returns a new context with a flag that
// suppresses providing for a specific call to NotifyNewBlocks.
func ContextWithSuppressProvide(ctx context.Context) context.Context {
	return context.WithValue(ctx, WithContextProvideKey{}, true)
}

// ShouldSuppressProvide checks if providing should be suppressed for the given context.
func ShouldSuppressProvide(ctx context.Context) bool {
	val, ok := ctx.Value(WithContextProvideKey{}).(bool)
	return ok && val
}
