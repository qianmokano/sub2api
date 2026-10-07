package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type ssoChallengeCache struct{ client *redis.Client }

func NewSSOChallengeCache(client *redis.Client) service.SSOChallengeStore {
	return &ssoChallengeCache{client: client}
}

func (s *ssoChallengeCache) Set(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if s.client == nil {
		return false, service.ErrServiceUnavailable
	}
	return s.client.SetNX(ctx, key, value, ttl).Result()
}

func (s *ssoChallengeCache) Take(ctx context.Context, key string) (string, bool, error) {
	if s.client == nil {
		return "", false, service.ErrServiceUnavailable
	}
	value, err := s.client.GetDel(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	return value, err == nil, err
}

func ssoChallengeKeys(token string) []string {
	prefix := "sso:mfa:{" + token + "}:"
	return []string{prefix + "session", prefix + "lock", prefix + "attempts"}
}

func (s *ssoChallengeCache) Put(ctx context.Context, token string, session *casdoor.Session) error {
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	created, err := s.client.SetNX(ctx, ssoChallengeKeys(token)[0], body, 10*time.Minute).Result()
	if err != nil {
		return service.ErrServiceUnavailable
	}
	if !created {
		return service.ErrSSOChallengeInvalid
	}
	return nil
}

var lockSSOChallenge = redis.NewScript(`
local session = redis.call('GET', KEYS[1])
if not session then return '' end
if not redis.call('SET', KEYS[2], ARGV[1], 'NX', 'PX', 30000) then return 'busy' end
local attempts = redis.call('INCR', KEYS[3])
redis.call('PEXPIRE', KEYS[3], 600000)
if attempts > 10 then
  redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
  return ''
end
return session
`)

func (s *ssoChallengeCache) Lock(ctx context.Context, token, lease string) (*casdoor.Session, error) {
	body, err := lockSSOChallenge.Run(ctx, s.client, ssoChallengeKeys(token), lease).Text()
	if err != nil {
		return nil, service.ErrServiceUnavailable
	}
	if body == "busy" {
		return nil, service.ErrSSOChallengeBusy
	}
	if body == "" {
		return nil, service.ErrSSOChallengeInvalid
	}
	var session casdoor.Session
	if json.Unmarshal([]byte(body), &session) != nil {
		return nil, service.ErrSSOChallengeInvalid
	}
	return &session, nil
}

var unlockSSOChallenge = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

var updateSSOChallenge = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return 0 end
local ttl = redis.call('PTTL', KEYS[1])
if ttl <= 0 then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ttl)
return 1
`)

func (s *ssoChallengeCache) Update(ctx context.Context, token, lease string, session *casdoor.Session) error {
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	updated, err := updateSSOChallenge.Run(ctx, s.client, ssoChallengeKeys(token), lease, body).Int()
	if err != nil {
		return service.ErrServiceUnavailable
	}
	if updated != 1 {
		return service.ErrSSOChallengeInvalid
	}
	return nil
}

func (s *ssoChallengeCache) Unlock(ctx context.Context, token, lease string) error {
	return unlockSSOChallenge.Run(ctx, s.client, []string{ssoChallengeKeys(token)[1]}, lease).Err()
}

var consumeSSOChallenge = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] or redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
return 1
`)

func (s *ssoChallengeCache) Consume(ctx context.Context, token, lease string) (bool, error) {
	result, err := consumeSSOChallenge.Run(ctx, s.client, ssoChallengeKeys(token), lease).Int()
	return result == 1, err
}
