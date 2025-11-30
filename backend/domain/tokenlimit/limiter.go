/*
 * Copyright 2025 coze-dev Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package tokenlimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coze-dev/coze-studio/backend/infra/cache"
)

const (
	defaultWindow = 18 * time.Hour
	defaultLimit  = int64(100000)
)

type limiter interface {
	allow(operatorID int64) bool
	record(operatorID int64, tokens int64)
	usageValue(operatorID int64) int64
	limitValue() int64
	windowSize() time.Duration
}

var current limiter = newMemoryLimiter(defaultWindow, defaultLimit)

// Init installs a Redis-backed limiter. If cli is nil the in-memory limiter remains.
func Init(cli cache.Cmdable) {
	if cli == nil {
		return
	}
	current = newRedisLimiter(cli, defaultWindow, defaultLimit)
}

// Allow reports whether the operator is still below the configured windowed quota.
func Allow(operatorID int64) bool {
	if operatorID == 0 {
		return true
	}
	return current.allow(operatorID)
}

// Record appends a finished execution's token usage into the sliding window.
func Record(operatorID int64, tokens int64) {
	if operatorID == 0 || tokens <= 0 {
		return
	}
	current.record(operatorID, tokens)
}

// Usage returns the aggregated token usage in the current sliding window.
func Usage(operatorID int64) int64 {
	if operatorID == 0 {
		return 0
	}
	return current.usageValue(operatorID)
}

// Limit reports the configured token threshold in the current limiter.
func Limit() int64 {
	return current.limitValue()
}

// Window reports the sliding window length.
func Window() time.Duration {
	return current.windowSize()
}

// memoryLimiter is a best-effort fallback used before Redis is initialized.
type memoryLimiter struct {
	windowDur time.Duration
	limitVal  int64

	mu        sync.Mutex
	usageLogs map[int64][]usageEntry
}

type usageEntry struct {
	ts     time.Time
	tokens int64
}

func newMemoryLimiter(window time.Duration, limit int64) *memoryLimiter {
	return &memoryLimiter{
		windowDur: window,
		limitVal:  limit,
		usageLogs: make(map[int64][]usageEntry),
	}
}

func (l *memoryLimiter) allow(operatorID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.pruneLocked(operatorID, time.Now())
	var total int64
	for _, entry := range entries {
		total += entry.tokens
	}

	return total < l.limitVal
}

func (l *memoryLimiter) record(operatorID int64, tokens int64) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.pruneLocked(operatorID, now)
	entries = append(entries, usageEntry{
		ts:     now,
		tokens: tokens,
	})
	l.usageLogs[operatorID] = entries
}

func (l *memoryLimiter) usageValue(operatorID int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var total int64
	for _, entry := range l.pruneLocked(operatorID, time.Now()) {
		total += entry.tokens
	}
	return total
}
func (l *memoryLimiter) limitValue() int64 {
	return l.limitVal
}

func (l *memoryLimiter) windowSize() time.Duration {
	return l.windowDur
}

func (l *memoryLimiter) pruneLocked(operatorID int64, now time.Time) []usageEntry {
	entries, ok := l.usageLogs[operatorID]
	if !ok || len(entries) == 0 {
		return nil
	}

	cutoff := now.Add(-l.windowDur)
	idx := 0
	for idx < len(entries) && entries[idx].ts.Before(cutoff) {
		idx++
	}

	switch {
	case idx == 0:
		return entries
	case idx >= len(entries):
		delete(l.usageLogs, operatorID)
		return nil
	default:
		trimmed := make([]usageEntry, len(entries)-idx)
		copy(trimmed, entries[idx:])
		l.usageLogs[operatorID] = trimmed
		return trimmed
	}
}

type redisLimiter struct {
	windowDur time.Duration
	limitVal  int64
	cli       cache.Cmdable
	ttl       time.Duration
	nowFunc   func() time.Time
}

func newRedisLimiter(cli cache.Cmdable, window time.Duration, limit int64) *redisLimiter {
	return &redisLimiter{
		windowDur: window,
		limitVal:  limit,
		cli:       cli,
		ttl:       window * 2,
		nowFunc:   time.Now,
	}
}

func (l *redisLimiter) allow(operatorID int64) bool {
	usage, err := l.currentUsage(operatorID)
	if err != nil {
		return true
	}
	return usage < l.limitVal
}

func (l *redisLimiter) record(operatorID int64, tokens int64) {
	if tokens <= 0 {
		return
	}
	ctx := context.Background()
	entry := fmt.Sprintf("%d:%d", l.nowFunc().UnixMilli(), tokens)
	keyEntries := l.entriesKey(operatorID)
	keyTotal := l.totalKey(operatorID)

	if _, err := l.cli.RPush(ctx, keyEntries, entry).Result(); err != nil {
		return
	}
	l.cli.Expire(ctx, keyEntries, l.ttl)

	if _, err := l.cli.IncrBy(ctx, keyTotal, tokens).Result(); err != nil {
		return
	}
	l.cli.Expire(ctx, keyTotal, l.ttl)

	_, _ = l.currentUsage(operatorID)
}

func (l *redisLimiter) usageValue(operatorID int64) int64 {
	usage, err := l.currentUsage(operatorID)
	if err != nil {
		return 0
	}
	return usage
}

func (l *redisLimiter) limitValue() int64 {
	return l.limitVal
}

func (l *redisLimiter) windowSize() time.Duration {
	return l.windowDur
}

func (l *redisLimiter) currentUsage(operatorID int64) (int64, error) {
	ctx := context.Background()
	cutoff := l.nowFunc().Add(-l.windowDur).UnixMilli()
	keyEntries := l.entriesKey(operatorID)
	keyTotal := l.totalKey(operatorID)

	for {
		entry, err := l.cli.LIndex(ctx, keyEntries, 0).Result()
		if err != nil {
			if errors.Is(err, cache.Nil) {
				break
			}
			return 0, err
		}
		ts, tokens, parseErr := parseEntry(entry)
		if parseErr != nil {
			_, _ = l.cli.LPop(ctx, keyEntries).Result()
			continue
		}
		if ts >= cutoff {
			break
		}
		if _, err := l.cli.LPop(ctx, keyEntries).Result(); err != nil && !errors.Is(err, cache.Nil) {
			return 0, err
		}
		_, _ = l.cli.IncrBy(ctx, keyTotal, -tokens).Result()
	}

	total, err := l.cli.Get(ctx, keyTotal).Int64()
	if err != nil {
		if errors.Is(err, cache.Nil) {
			return 0, nil
		}
		return 0, err
	}
	if total < 0 {
		l.cli.Set(ctx, keyTotal, 0, l.ttl)
		return 0, nil
	}

	return total, nil
}

func (l *redisLimiter) entriesKey(operatorID int64) string {
	return fmt.Sprintf("tokenlimit:%d:entries", operatorID)
}

func (l *redisLimiter) totalKey(operatorID int64) string {
	return fmt.Sprintf("tokenlimit:%d:total", operatorID)
}

func parseEntry(entry string) (timestamp int64, tokens int64, err error) {
	parts := strings.SplitN(entry, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid entry: %s", entry)
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	tokenVal, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return ts, tokenVal, nil
}
