use std::{collections::HashMap, sync::Arc};

use tokio::sync::{Mutex as AsyncMutex, OwnedMutexGuard};

#[derive(Clone, Default)]
pub struct ConversationLockManager {
    inner: Arc<Inner>,
}

#[derive(Default)]
struct Inner {
    entries: std::sync::Mutex<HashMap<String, Entry>>,
}

struct Entry {
    token: Arc<AsyncMutex<()>>,
    references: usize,
}

struct Registration {
    manager: Arc<Inner>,
    key: String,
    token: Arc<AsyncMutex<()>>,
}

pub struct ConversationGuard {
    _guard: OwnedMutexGuard<()>,
    _registration: Registration,
}

impl ConversationLockManager {
    pub fn new() -> Self {
        Self::default()
    }

    pub async fn lock(&self, channel_id: &str, sender_id: &str) -> ConversationGuard {
        let key = format!("{channel_id}\0{sender_id}");
        let token = {
            let mut entries = self
                .inner
                .entries
                .lock()
                .expect("conversation lock poisoned");
            let entry = entries.entry(key.clone()).or_insert_with(|| Entry {
                token: Arc::new(AsyncMutex::new(())),
                references: 0,
            });
            entry.references += 1;
            Arc::clone(&entry.token)
        };
        let registration = Registration {
            manager: Arc::clone(&self.inner),
            key,
            token: Arc::clone(&token),
        };
        let guard = token.lock_owned().await;
        ConversationGuard {
            _guard: guard,
            _registration: registration,
        }
    }

    #[cfg(test)]
    fn entry_count(&self) -> usize {
        self.inner
            .entries
            .lock()
            .expect("conversation lock poisoned")
            .len()
    }
}

impl Drop for Registration {
    fn drop(&mut self) {
        let mut entries = self
            .manager
            .entries
            .lock()
            .expect("conversation lock poisoned");
        let Some(entry) = entries.get_mut(&self.key) else {
            return;
        };
        if !Arc::ptr_eq(&entry.token, &self.token) {
            return;
        }
        entry.references -= 1;
        if entry.references == 0 {
            entries.remove(&self.key);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[tokio::test]
    async fn serializes_one_route_and_allows_distinct_routes() {
        let manager = ConversationLockManager::new();
        let first = manager.lock("telegram", "one").await;
        let acquired = Arc::new(AtomicUsize::new(0));

        let same_manager = manager.clone();
        let same_acquired = Arc::clone(&acquired);
        let same = tokio::spawn(async move {
            let _guard = same_manager.lock("telegram", "one").await;
            same_acquired.fetch_add(1, Ordering::SeqCst);
        });
        let other_manager = manager.clone();
        let other_acquired = Arc::clone(&acquired);
        let other = tokio::spawn(async move {
            let _guard = other_manager.lock("telegram", "two").await;
            other_acquired.fetch_add(10, Ordering::SeqCst);
        });
        tokio::task::yield_now().await;
        assert_eq!(acquired.load(Ordering::SeqCst), 10);
        drop(first);
        same.await.unwrap();
        other.await.unwrap();
        assert_eq!(acquired.load(Ordering::SeqCst), 11);
        assert_eq!(manager.entry_count(), 0);
    }

    #[tokio::test]
    async fn cancellation_releases_reference() {
        let manager = ConversationLockManager::new();
        let first = manager.lock("telegram", "owner").await;
        let waiting_manager = manager.clone();
        let waiting = tokio::spawn(async move {
            let _guard = waiting_manager.lock("telegram", "owner").await;
        });
        tokio::task::yield_now().await;
        waiting.abort();
        assert!(waiting.await.unwrap_err().is_cancelled());
        assert_eq!(manager.entry_count(), 1);
        drop(first);
        assert_eq!(manager.entry_count(), 0);
    }
}
