package dag

import "context"

type deltaSinkKey struct{}

// WithDeltaSink attaches a receiver for streamed model text to ctx. Providers that stream call
// it for every text delta, in addition to their own OnDelta hook, so a surface can stream a
// branch's output without rebuilding providers.
func WithDeltaSink(ctx context.Context, fn func(text string)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, deltaSinkKey{}, fn)
}

// deltaSink combines the provider's own hook with the context sink.
func deltaSink(ctx context.Context, own func(string)) func(string) {
	ctxFn, _ := ctx.Value(deltaSinkKey{}).(func(string))
	switch {
	case ctxFn == nil:
		return own
	case own == nil:
		return ctxFn
	default:
		return func(t string) { own(t); ctxFn(t) }
	}
}
