package session

import (
	"context"
	"errors"

	"github.com/marang/sway-session/internal/swayipc"
)

func lifecycleCompositorID(ctx context.Context, client SwayRequestClient) (string, error) {
	provider, ok := client.(interface {
		LifecycleCompositorID(context.Context) (string, error)
	})
	if !ok {
		return "", errors.New("durable application mutation requires a compositor lifetime identity")
	}
	identity, err := provider.LifecycleCompositorID(ctx)
	if err != nil {
		if errors.Is(err, swayipc.ErrCompositorChanged) {
			return "", lifecycleConflictReason("compositor_changed", err.Error())
		}
		return "", err
	}
	if identity == "" {
		return "", errors.New("compositor lifetime identity is empty")
	}
	return identity, nil
}

// lifecycleClientScope keeps ordinary daemon connections independent from the
// lifetime pin of a single durable operation. Fakes implement the identity
// boundary directly; real IPC clients always provide a separately pinned scope.
func lifecycleClientScope(ctx context.Context, client SwayRequestClient) (SwayRequestClient, func(), error) {
	if provider, ok := client.(interface {
		PinCompositor(context.Context) (*swayipc.Client, error)
	}); ok {
		scoped, err := provider.PinCompositor(ctx)
		if err != nil {
			if errors.Is(err, swayipc.ErrCompositorChanged) {
				return nil, func() {}, lifecycleConflictReason("compositor_changed", err.Error())
			}
			return nil, func() {}, err
		}
		return scoped, scoped.Close, nil
	}
	return client, func() {}, nil
}
