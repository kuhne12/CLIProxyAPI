package bpsbridge

import "context"

// Observer records local transport boundaries without changing the protocol.
// Callbacks run synchronously, must return promptly, and must not mutate or retain
// event bytes. Events may contain credentials or private content.
type Observer struct {
	DialStart  func()
	DialDone   func(status int)
	WriteStart func()
	WriteDone  func()
	Event      func([]byte)
}

type observerKey struct{}

func WithObserver(ctx context.Context, observer Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, observer)
}

func observerFromContext(ctx context.Context) Observer {
	observer, _ := ctx.Value(observerKey{}).(Observer)
	return observer
}
