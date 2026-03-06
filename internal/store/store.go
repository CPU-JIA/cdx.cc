package store

import "context"

type Store interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key string, value string) error
	Close() error
}
