package gateway

import (
	"context"
	"sync"
)

type conversationLockEntry struct {
	token chan struct{}
	refs  int
}

type conversationLockManager struct {
	mu      sync.Mutex
	entries map[string]*conversationLockEntry
}

func newConversationLockManager() *conversationLockManager {
	return &conversationLockManager{entries: make(map[string]*conversationLockEntry)}
}

func conversationLockKey(channelID, senderID string) string {
	return channelID + "\x00" + senderID
}

func (manager *conversationLockManager) lock(ctx context.Context, key string) (func(), error) {
	manager.mu.Lock()
	entry := manager.entries[key]
	if entry == nil {
		entry = &conversationLockEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		manager.entries[key] = entry
	}
	entry.refs++
	manager.mu.Unlock()

	select {
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			manager.releaseRef(key, entry)
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				entry.token <- struct{}{}
				manager.releaseRef(key, entry)
			})
		}, nil
	case <-ctx.Done():
		manager.releaseRef(key, entry)
		return nil, ctx.Err()
	}
}

func (manager *conversationLockManager) releaseRef(key string, entry *conversationLockEntry) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && manager.entries[key] == entry {
		delete(manager.entries, key)
	}
}
