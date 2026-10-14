package kanoagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisEntry struct {
	Digest  string `json:"digest"`
	Type    string `json:"type"`
	Expires int64  `json:"expires"`
}

type RedisSnapshot []map[string]RedisEntry

type RedisBackend interface {
	Snapshot(context.Context, Container, Container) (RedisSnapshot, error)
	Save(context.Context, Container) error
	Ready(context.Context, Container, Container) error
}

type nativeRedis struct{}

func (nativeRedis) Ready(ctx context.Context, container, identity Container) error {
	client, err := redisClient(container, identity, 0)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return waitRedis(ctx, client)
}

func (nativeRedis) Snapshot(ctx context.Context, container, identity Container) (RedisSnapshot, error) {
	return snapshotRedis(ctx, container, identity)
}
func (nativeRedis) Save(ctx context.Context, container Container) error {
	return saveRedis(ctx, container)
}

func (e *Engine) redisBackend() RedisBackend {
	if e.Redis != nil {
		return e.Redis
	}
	return nativeRedis{}
}

func redisPassword(container Container) string {
	environment := container.Environment()
	password := environment["REDISCLI_AUTH"]
	if password == "" {
		password = environment["REDIS_PASSWORD"]
	}
	if password == "" {
		for index, part := range container.Config.Cmd {
			if part == "--requirepass" && index+1 < len(container.Config.Cmd) {
				password = container.Config.Cmd[index+1]
				break
			}
		}
	}
	return password
}

func redisAddress(container Container) (string, error) {
	port := "6379"
	for index, part := range container.Config.Cmd {
		if part == "--port" {
			if index+1 >= len(container.Config.Cmd) {
				return "", errors.New("Redis port is missing")
			}
			port = container.Config.Cmd[index+1]
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				return "", errors.New("Redis port is invalid")
			}
		}
	}
	for _, name := range sortedKeys(container.NetworkSettings.Networks) {
		address := container.NetworkSettings.Networks[name].IPAddress
		if address != "" {
			if net.ParseIP(address) == nil {
				return "", errors.New("invalid Redis container address")
			}
			return net.JoinHostPort(address, port), nil
		}
	}
	return "", errors.New("Redis container has no private network address")
}

func redisClient(container, identity Container, db int) (*redis.Client, error) {
	address, err := redisAddress(container)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(&redis.Options{Addr: address, Password: redisPassword(identity), DB: db, DialTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, Protocol: 2, MaxRetries: 1}), nil
}

func waitRedis(ctx context.Context, client *redis.Client) error {
	for attempt := 0; attempt < 60; attempt++ {
		err := client.Ping(ctx).Err()
		if err == nil {
			return nil
		}
		if !strings.HasPrefix(err.Error(), "LOADING ") {
			return errors.New("Redis backup connection unavailable")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("Redis restore readiness timed out")
}

func canonicalRedis(ctx context.Context, client *redis.Client, key, kind string) (string, error) {
	var value any
	switch kind {
	case "string":
		text, err := client.Get(ctx, key).Bytes()
		if err != nil {
			return "", err
		}
		value = hex.EncodeToString(text)
	case "hash":
		fields, err := client.HGetAll(ctx, key).Result()
		if err != nil {
			return "", err
		}
		entries := make([][]string, 0, len(fields))
		for _, field := range sortedKeys(fields) {
			entries = append(entries, []string{hex.EncodeToString([]byte(field)), hex.EncodeToString([]byte(fields[field]))})
		}
		value = entries
	case "set":
		entries, err := client.SMembers(ctx, key).Result()
		if err != nil {
			return "", err
		}
		sort.Strings(entries)
		for index, entry := range entries {
			entries[index] = hex.EncodeToString([]byte(entry))
		}
		value = entries
	case "list":
		entries, err := client.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			return "", err
		}
		for index, entry := range entries {
			entries[index] = hex.EncodeToString([]byte(entry))
		}
		value = entries
	case "zset":
		entries, err := client.ZRangeWithScores(ctx, key, 0, -1).Result()
		if err != nil {
			return "", err
		}
		values := make([][]string, 0, len(entries))
		for _, entry := range entries {
			member, ok := entry.Member.(string)
			if !ok {
				return "", errors.New("unexpected sorted set member")
			}
			values = append(values, []string{hex.EncodeToString([]byte(member)), strconv.FormatFloat(entry.Score, 'g', -1, 64)})
		}
		value = values
	default:
		return "", errors.New("unsupported Redis data type for complete restore verification")
	}
	body, err := json.Marshal(map[string]any{"type": kind, "value": value})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func snapshotRedis(ctx context.Context, container, identity Container) (RedisSnapshot, error) {
	client, err := redisClient(container, identity, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	if err = waitRedis(ctx, client); err != nil {
		return nil, err
	}
	configuration, err := client.ConfigGet(ctx, "databases").Result()
	if err != nil {
		return nil, errors.New("Redis database catalog is unavailable")
	}
	count, err := strconv.Atoi(configuration["databases"])
	if err != nil || count < 1 || count > 1024 {
		return nil, errors.New("unsupported Redis database count")
	}
	result := make(RedisSnapshot, count)
	for database := 0; database < count; database++ {
		databaseClient, clientErr := redisClient(container, identity, database)
		if clientErr != nil {
			return nil, clientErr
		}
		entries, snapshotErr := snapshotRedisDatabase(ctx, databaseClient)
		_ = databaseClient.Close()
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		result[database] = entries
	}
	return result, nil
}

func snapshotRedisDatabase(ctx context.Context, client *redis.Client) (map[string]RedisEntry, error) {
	if err := waitRedis(ctx, client); err != nil {
		return nil, err
	}
	entries := map[string]RedisEntry{}
	var cursor uint64
	for {
		keys, next, scanErr := client.Scan(ctx, cursor, "*", 1000).Result()
		if scanErr != nil {
			return nil, errors.New("Redis key snapshot is unavailable")
		}
		for _, key := range keys {
			kind, typeErr := client.Type(ctx, key).Result()
			if typeErr != nil {
				return nil, errors.New("Redis key type is unavailable")
			}
			if kind == "none" {
				continue
			}
			digest, valueErr := canonicalRedis(ctx, client, key, kind)
			if errors.Is(valueErr, redis.Nil) {
				continue
			}
			if valueErr != nil {
				return nil, errors.New("Redis value cannot be completely verified")
			}
			expires, expireErr := client.Do(ctx, "PEXPIRETIME", key).Int64()
			if expireErr != nil {
				return nil, errors.New("Redis absolute expiry is unavailable")
			}
			if expires == -2 {
				continue
			}
			entries[hex.EncodeToString([]byte(key))] = RedisEntry{Digest: digest, Type: kind, Expires: expires}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return entries, nil
}

func compareRedis(before, after RedisSnapshot, now time.Time) error {
	if len(before) != len(after) {
		return errors.New("Redis database count changed")
	}
	for database, expected := range before {
		for key, entry := range expected {
			actual, ok := after[database][key]
			if !ok && entry.Expires >= 0 && entry.Expires <= now.UnixMilli() {
				continue
			}
			if !ok || actual != entry {
				return errors.New("restored Redis value or absolute expiry differs")
			}
		}
		for key := range after[database] {
			if _, ok := expected[key]; !ok {
				return errors.New("unexpected restored Redis key")
			}
		}
	}
	return nil
}

func saveRedis(ctx context.Context, container Container) error {
	client, err := redisClient(container, container, 0)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err = waitRedis(ctx, client); err != nil {
		return err
	}
	if err = client.Save(ctx).Err(); err != nil {
		return errors.New("Redis snapshot failed")
	}
	return nil
}
