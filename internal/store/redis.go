package store

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
}

func NewRedisStore(redisURL string, ttl time.Duration) (*RedisStore, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	return &RedisStore{
		client: redis.NewClient(opts),
		ttl:    ttl,
	}, nil
}

func (r *RedisStore) Get(ctx context.Context, key string) (string, bool, error) {
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

func (r *RedisStore) Set(ctx context.Context, key string, value string) error {
	return r.client.Set(ctx, key, value, r.ttl).Err()
}

func (r *RedisStore) Close() error {
	return r.client.Close()
}
