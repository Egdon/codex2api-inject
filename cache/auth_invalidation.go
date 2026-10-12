package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// AuthInvalidationBus is an optional latency optimization. Consumers must keep
// a durable revision check: Redis Pub/Sub does not replay missed notifications.
type AuthInvalidationBus interface {
	PublishAuthInvalidation(context.Context, string) error
	SubscribeAuthInvalidations(context.Context, string, func()) error
}

// BoundedRuntimeReader avoids fetching an arbitrarily large corrupted snapshot.
type BoundedRuntimeReader interface {
	GetRuntimeBounded(context.Context, string, string, int64) ([]byte, bool, error)
}

// ErrRuntimeSnapshotTooLarge 表示后端值在反序列化前超过读取上限。
var ErrRuntimeSnapshotTooLarge = errors.New("runtime snapshot exceeds byte limit")

type AuthSnapshotWriter interface {
	SetAuthSnapshot(context.Context, string, string, json.RawMessage, time.Duration) error
}

func authOperationTimeout(ctx context.Context) time.Duration {
	timeout := AuthInvalidationTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	return max(time.Nanosecond, timeout)
}

func authInvalidationChannel(scope string) string {
	return runtimeHashKey("codex:auth-invalidate:", scope)
}

func (tc *redisTokenCache) PublishAuthInvalidation(ctx context.Context, scope string) error {
	return tc.client.WithTimeout(authOperationTimeout(ctx)).Publish(ctx, authInvalidationChannel(scope), "invalidate").Err()
}

func (tc *redisTokenCache) SubscribeAuthInvalidations(ctx context.Context, scope string, invalidate func()) error {
	pubsub := tc.client.WithTimeout(AuthInvalidationTimeout).Subscribe(ctx)
	defer pubsub.Close()
	startCtx, cancel := context.WithTimeout(ctx, AuthInvalidationTimeout)
	err := pubsub.Subscribe(startCtx, authInvalidationChannel(scope))
	cancel()
	if err != nil {
		return err
	}
	// Subscription acknowledgements include reconnects. Reading them through
	// the channel also lets cancellation close a stalled initial subscription.
	ch := pubsub.ChannelWithSubscriptions()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-ch:
			if !ok {
				return errors.New("auth invalidation subscription closed")
			}
			invalidate()
		}
	}
}

func (tc *redisTokenCache) GetRuntimeBounded(ctx context.Context, namespace, key string, maxBytes int64) ([]byte, bool, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	raw, err := tc.client.WithTimeout(authOperationTimeout(ctx)).GetRange(ctx, runtimeValueKey(namespace, key), 0, maxBytes).Bytes()
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, false, ErrRuntimeSnapshotTooLarge
	}
	return raw, len(raw) > 0, nil
}

func (tc *redisTokenCache) SetAuthSnapshot(ctx context.Context, namespace, key string, raw json.RawMessage, ttl time.Duration) error {
	return tc.client.WithTimeout(authOperationTimeout(ctx)).Set(ctx, runtimeValueKey(namespace, key), []byte(raw), ttl).Err()
}

var _ AuthInvalidationBus = (*redisTokenCache)(nil)
var _ BoundedRuntimeReader = (*redisTokenCache)(nil)

const AuthInvalidationTimeout = 300 * time.Millisecond
