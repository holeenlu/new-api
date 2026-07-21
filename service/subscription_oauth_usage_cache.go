package service

import (
	"sync"
	"time"
)

const maxSubscriptionOAuthUsageEvidenceEntries = 256

type subscriptionOAuthUsageEvidenceEntry[T any] struct {
	value     T
	expiresAt time.Time
}

// subscriptionOAuthUsageEvidenceCache retains only short-lived correlation
// evidence. Credential cooldown and routing availability remain owned by the
// subscription OAuth capacity state machine.
type subscriptionOAuthUsageEvidenceCache[T any] struct {
	mu         sync.Mutex
	entries    map[string]subscriptionOAuthUsageEvidenceEntry[T]
	maxEntries int
	now        func() time.Time
}

func newSubscriptionOAuthUsageEvidenceCache[T any]() *subscriptionOAuthUsageEvidenceCache[T] {
	return &subscriptionOAuthUsageEvidenceCache[T]{
		entries:    make(map[string]subscriptionOAuthUsageEvidenceEntry[T]),
		maxEntries: maxSubscriptionOAuthUsageEvidenceEntries,
		now:        time.Now,
	}
}

func (c *subscriptionOAuthUsageEvidenceCache[T]) get(key string) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if !entry.expiresAt.After(now) {
		delete(c.entries, key)
		return zero, false
	}
	return entry.value, true
}

func (c *subscriptionOAuthUsageEvidenceCache[T]) put(key string, value T, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	for entryKey, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, entryKey)
		}
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxEntries {
		oldestKey := ""
		var oldestExpiry time.Time
		for entryKey, entry := range c.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
				oldestKey = entryKey
				oldestExpiry = entry.expiresAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = subscriptionOAuthUsageEvidenceEntry[T]{
		value:     value,
		expiresAt: now.Add(ttl),
	}
}
