//! A cache for [`crate::page_server::mgr`]+`Tenant::get_timeline`+`Timeline::gate.enter()`.
//!
//! # Motivation
//!
//! On a single page service connection, we're serving a single TenantTimelineId.
//!
//! There is a single Timeline object to which we dispatch all requests. For
//! example, a getpage request gets dispatched to the Timeline::get method of
//! the Timeline object that represents the (tenant,timeline) of that
//! connection.
//!
//! We want to ensure that we hold the Timeline gate open while we're invoking
//! the method on the Timeline object.
//!
//! We want to avoid the overhead of doing, for each incoming request,
//! - tenant manager lookup (global rwlock + btreemap lookup)
//! - cloning the `Arc<Timeline>` out of the tenant manager so we can
//!   release the mgr rwlock before doing any request processing work
//! - re-entering the Timeline gate for each Timeline method invocation.
//!
//! Regardless of how we accomplish the above, it should not
//! prevent the Timeline from shutting down promptly.
//!
//!
//! # Design
//!
//! ## Data Structures
//!
//! There are two concepts expressed as associated types in the `Types` trait:
//! - `TenantManager`: the thing that performs the expensive work. It produces
//!   a `Timeline` object, which is the other associated type.
//! - `Timeline`: the item that we cache for fast TimelineId lookup.
//!
//! There are three user-facing data structures exposed by this module:
//! - `PerTimelineState`: a struct embedded into each Timeline struct. Lifetime == Timeline lifetime.
//! - `Cache`: a struct private to each connection handler; Lifetime == connection lifetime.
//! - `Handle`: a smart pointer that derefs to the Types::Timeline.
//! - `WeakHandle`: downgrade of a `Handle` that does not keep the gate open, but allows
//!   trying to ugprade back to a `Handle`. If successful, a re-upgraded Handle will always
//!   point to the same cached `Types::Timeline`. Upgrades never invoke the `TenantManager`.
//!
//! Internally, there is 0 or 1 `HandleInner` per `(Cache,Timeline)`.
//! Since Cache:Connection is 1:1, there is 0 or 1 `HandleInner` per `(Connection,Timeline)`.
//!
//! The `HandleInner`  is allocated as a `Arc<Mutex<HandleInner>>` and
//! referenced weakly and strongly from various places which we are now illustrating.
//! For brevity, we will omit the `Arc<Mutex<>>` part in the following and instead
//! use `strong ref` and `weak ref` when referring to the `Arc<Mutex<HandleInner>>`
//! or `Weak<Mutex<HandleInner>>`, respectively.
//!
//! - The `Handle` is a strong ref.
//! - The `WeakHandle` is a weak ref.
//! - The `PerTimelineState` contains a `HashMap<CacheId, strong ref>`.
//! - The `Cache` is a `HashMap<TimelineId, weak ref>`.
//!
//! Lifetimes:
//! - `WeakHandle` and `Handle`: single pagestream request.
//! - `Cache`: single page service connection.
//! - `PerTimelineState`:  lifetime of the Timeline object (i.e., i.e., till `Timeline::shutdown`).
//!
//! ## Request Handling Flow (= filling and using the `Cache``)
//!
//! To dispatch a request, the page service connection calls `Cache::get`.
//!
//! A cache miss means we call Types::TenantManager::resolve,
//! cloning the `Arc<Timeline>` out of it, and entering the gate. The result of
//! resolve() is the object we want to cache, and return `Handle`s to for subseqent `Cache::get` calls.
//!
//! We wrap the object returned from resolve() in an `Arc` and store that inside the
//! `Arc<Mutex<HandleInner>>>`. A weak ref to the HandleInner is stored in the `Cache`
//! and a strong ref in the `PerTimelineState`.
//! Another strong ref is returned wrapped in a `Handle`.
//!
//! For subsequent requests, `Cache::get` will find the weak ref in the cache.
//! We upgrade the weak ref to a strong ref and return it wrapped in a `Handle`.
//!
//! The pagestream processing is pipelined and involves a batching step.
//! While a request is batching, the `Handle` is downgraded to a `WeakHandle`.
//! When the batch is ready to be executed, the `WeakHandle` is upgraded back to a `Handle`
//! and the request handler dispatches the request to the right `<Handle as Deref<Target = Timeline>>::$request_method`.
//! It then drops the `Handle`, and thus the `Arc<Mutex<HandleInner>>` inside it.
//!
//! # Performance
//!
//! Remember from the introductory section:
//!
//! > We want to avoid the overhead of doing, for each incoming request,
//! > - tenant manager lookup (global rwlock + btreemap lookup)
//! > - cloning the `Arc<Timeline>` out of the tenant manager so we can
//! >   release the mgr rwlock before doing any request processing work
//! > - re-entering the Timeline gate for each Timeline method invocation.
//!
//! All of these boil down to some state that is either globally shared
//! or state shared among all tasks that serve a particular timeline.
//! It is either protected by RwLock or manipulated via atomics.
//! Even atomics are costly when shared across multiple cores.
//! So, we want to avoid any permanent need for coordination between page_service tasks.
//!
//! The solution is to add indirection: we wrap the Types::Timeline object that is
//! returned by Types::TenantManager into an Arc that is rivate to the `HandleInner`
//! and hence to the single Cache / page_service connection.
//! (Review the "Data Structures" section if that is unclear to you.)
//!
//!
//! When upgrading a `WeakHandle`, we upgrade its weak to a strong ref (of the `Mutex<HandleInner>`),
//! lock the mutex, take out a clone of the `Arc<Types::Timeline>`, and drop the Mutex.
//! The Mutex is not contended because it is private to the connection.
//! And again, the  `Arc<Types::Timeline>` clone is cheap because that wrapper
//! Arc's refcounts are private to the connection.
//!
//! Downgrading drops these two Arcs, which again, manipulates refcounts that are private to the connection.
//!
//!
//! # Shutdown
//!
//! The attentive reader may have noticed the following reference cycle around the `Arc<Timeline>`:
//!
//! ```text
//! Timeline --owns--> PerTimelineState --strong--> HandleInner --strong--> Types::Timeline --strong--> Timeline
//! ```
//!
//! Further, there is this cycle:
//!
//! ```text
//! Timeline --owns--> PerTimelineState --strong--> HandleInner --strong--> Types::Timeline --strong--> GateGuard --keepalive--> Timeline
//! ```
//!
//! The former cycle is a memory leak if not broken.
//! The latter cycle further prevents the Timeline from shutting down
//! because we certainly won't drop the Timeline while the GateGuard is alive.
//! Preventing shutdown is the whole point of this handle/cache system,
//! but when the Timeline needs to shut down, we need to break the cycle.
//!
//! The cycle is broken by either
//! - Timeline shutdown (=> `PerTimelineState::shutdown`)
//! - Connection shutdown (=> dropping the `Cache`).
//!
//! Both transition the `HandleInner` from [`HandleInner::Open`] to
//! [`HandleInner::ShutDown`], which drops the only long-lived
//! `Arc<Types::Timeline>`. Once the last short-lived Arc<Types::Timeline>
//! is dropped, the `Types::Timeline` gets dropped and thereby
//! the `GateGuard` and the `Arc<Timeline>` that it stores,
//! thereby breaking both cycles.
//!
//! `PerTimelineState::shutdown` drops all the `HandleInners` it contains,
//! thereby breaking the cycle.
//! It also initiates draining of already existing `Handle`s by
//! poisoning things so that no new `HandleInner`'s can be added
//! to the `PerTimelineState`, which will make subsequent `Cache::get` fail.
//!
//! Concurrently existing / already upgraded `Handle`s will extend the
//! lifetime of the `Arc<Mutex<HandleInner>>` and hence cycles.
//! However, since `Handle`s are short-lived and new `Handle`s are not
//! handed out from `Cache::get` or `WeakHandle::upgrade` after
//! `PerTimelineState::shutdown`, that extension of the cycle is bounded.
//!
//! Concurrently existing `WeakHandle`s will fail to `upgrade()`:
//! while they will succeed in upgrading `Weak<Mutex<HandleInner>>`,
//! they will find the inner in state `HandleInner::ShutDown` state where the
//! `Arc<GateGuard>` and Timeline has already been dropped.
//!
//! Dropping the `Cache` undoes the registration of this `Cache`'s
//! `HandleInner`s from all the `PerTimelineState`s, i.e., it
//! removes the strong ref to each of its `HandleInner`s
//! from all the `PerTimelineState`.
//!
//! # Locking Rules
//!
//! To prevent deadlocks we:
//!
//! 1. Only ever hold one of the locks at a time.
//! 2. Don't add more than one Drop impl that locks on the
//!    cycles above.
//!
//! As per (2), that impl is in `Drop for Cache`.
//!
//! # Cache lookup
//!
//! The `Cache` maintains a hash map of `TimelineId` to `WeakHandle`s, so a
//! repeat request for a timeline served on this connection avoids calling
//! into the tenant manager. It looks up the requested `TimelineId`; if the
//! `WeakHandle` is present and can be upgraded, it's a hit.
//!
//! ## Cache invalidation
//!
//! The insight is that cache invalidation is sufficient and most efficiently if done lazily.
//! The only reason an entry in the cache can become stale is that the
//! `PerTimelineState` / Timeline is shutting down (timeline deleted, or
//! pageserver is shutting down). We will eventually fail to upgrade the
//! `WeakHandle` once the timeline has shut down, and when that happens, we
//! remove the entry from the cache.

use std::collections::HashMap;
use std::collections::hash_map;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::Weak;

use tracing::{instrument, trace};
use utils::id::TimelineId;

use crate::page_service::GetActiveTimelineError;
use crate::page_server::GetTimelineError;

pub(crate) trait Types: Sized {
    type TenantManager: TenantManager<Self> + Sized;
    type Timeline: Timeline<Self> + Sized;
}

/// Uniquely identifies a [`Cache`] instance over the lifetime of the process.
/// Required so [`Cache::drop`] can take out the handles from the [`PerTimelineState`].
/// Alternative to this would be to allocate [`Cache`] in a `Box` and identify it by the pointer.
#[derive(Debug, Hash, PartialEq, Eq, Clone, Copy)]
struct CacheId(u64);

impl CacheId {
    fn next() -> Self {
        static NEXT_ID: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
        let id = NEXT_ID.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        if id == 0 {
            panic!("CacheId::new() returned 0, overflow");
        }
        Self(id)
    }
}

/// See module-level comment.
pub(crate) struct Cache<T: Types> {
    id: CacheId,
    map: Map<T>,
}

type Map<T> = HashMap<TimelineId, WeakHandle<T>>;

impl<T: Types> Default for Cache<T> {
    fn default() -> Self {
        Self {
            id: CacheId::next(),
            map: Default::default(),
        }
    }
}

/// See module-level comment.
pub(crate) struct Handle<T: Types> {
    inner: Arc<Mutex<HandleInner<T>>>,
    open: Arc<T::Timeline>,
}
pub(crate) struct WeakHandle<T: Types> {
    inner: Weak<Mutex<HandleInner<T>>>,
}

enum HandleInner<T: Types> {
    Open(Arc<T::Timeline>),
    ShutDown,
}

/// Embedded in each [`Types::Timeline`] as the anchor for the only long-lived strong ref to `HandleInner`.
///
/// See module-level comment for details.
pub struct PerTimelineState<T: Types> {
    // None = shutting down
    #[allow(clippy::type_complexity)]
    handles: Mutex<Option<HashMap<CacheId, Arc<Mutex<HandleInner<T>>>>>>,
}

impl<T: Types> Default for PerTimelineState<T> {
    fn default() -> Self {
        Self {
            handles: Mutex::new(Some(Default::default())),
        }
    }
}

/// Abstract view of [`crate::page_server::mgr`], for testability.
pub(crate) trait TenantManager<T: Types> {
    /// Invoked by [`Cache::get`] to resolve a [`TimelineId`] to a [`Types::Timeline`].
    async fn resolve(
        &self,
        timeline_id: TimelineId,
    ) -> Result<T::Timeline, GetActiveTimelineError>;
}

/// Abstract view of an [`Arc<Timeline>`], for testability.
pub(crate) trait Timeline<T: Types> {
    fn timeline_id(&self) -> TimelineId;
    fn per_timeline_state(&self) -> &PerTimelineState<T>;
}

impl<T: Types> Cache<T> {
    /// A wrapper of do_get to resolve a timeline for a get page request.
    /// The timeline is open before listeners start, so there is no
    /// wait-for-active retry: resolution either succeeds or fails fast.
    #[instrument(level = "trace", skip_all)]
    pub(crate) async fn get(
        &mut self,
        timeline_id: TimelineId,
        tenant_manager: &T::TenantManager,
    ) -> Result<Handle<T>, GetActiveTimelineError> {
        self.do_get(timeline_id, tenant_manager).await
    }

    /// See module-level comment for details.
    ///
    /// Does NOT check for the shutdown state of [`Types::Timeline`].
    /// Instead, the methods of [`Types::Timeline`] that are invoked through
    /// the [`Handle`] are responsible for checking these conditions
    /// and if so, return an error that causes the page service to
    /// close the connection.
    #[instrument(level = "trace", skip_all)]
    async fn do_get(
        &mut self,
        timeline_id: TimelineId,
        tenant_manager: &T::TenantManager,
    ) -> Result<Handle<T>, GetActiveTimelineError> {
        if let Some(cached) = self.map.get(&timeline_id) {
            match cached.upgrade() {
                Ok(upgraded) => return Ok(upgraded),
                Err(HandleUpgradeError::ShutDown) => {
                    trace!("handle cache stale");
                    self.map.remove(&timeline_id);
                }
            }
        }
        self.get_miss(timeline_id, tenant_manager).await
    }

    #[instrument(level = "trace", skip_all)]
    #[inline(always)]
    async fn get_miss(
        &mut self,
        timeline_id: TimelineId,
        tenant_manager: &T::TenantManager,
    ) -> Result<Handle<T>, GetActiveTimelineError> {
        let timeline = tenant_manager.resolve(timeline_id).await?;
        let key = timeline.timeline_id();

        trace!("creating new HandleInner");
        let timeline = Arc::new(timeline);
        let handle_inner_arc = Arc::new(Mutex::new(HandleInner::Open(Arc::clone(&timeline))));
        let handle_weak = WeakHandle {
            inner: Arc::downgrade(&handle_inner_arc),
        };
        let handle = handle_weak
            .upgrade()
            .ok()
            .expect("we just created it and it's not linked anywhere yet");
        let mut lock_guard = timeline
            .per_timeline_state()
            .handles
            .lock()
            .expect("mutex poisoned");
        let Some(per_timeline_state) = &mut *lock_guard else {
            return Err(GetActiveTimelineError::Timeline(
                GetTimelineError::ShuttingDown,
            ));
        };
        let replaced = per_timeline_state.insert(self.id, Arc::clone(&handle_inner_arc));
        assert!(replaced.is_none(), "some earlier code left a stale handle");
        match self.map.entry(key) {
            hash_map::Entry::Occupied(_o) => {
                // This cannot not happen because
                // 1. we're the _miss_ handle, i.e., `self.map` didn't contain an entry and
                // 2. we were holding &mut self during .resolve().await above, so, no other thread can have inserted a handle
                //    while we were waiting for the tenant manager.
                unreachable!()
            }
            hash_map::Entry::Vacant(v) => {
                v.insert(handle_weak);
            }
        }
        Ok(handle)
    }
}

pub(crate) enum HandleUpgradeError {
    ShutDown,
}

impl<T: Types> WeakHandle<T> {
    pub(crate) fn upgrade(&self) -> Result<Handle<T>, HandleUpgradeError> {
        let Some(inner) = Weak::upgrade(&self.inner) else {
            return Err(HandleUpgradeError::ShutDown);
        };
        let lock_guard = inner.lock().expect("poisoned");
        match &*lock_guard {
            HandleInner::Open(open) => {
                let open = Arc::clone(open);
                drop(lock_guard);
                Ok(Handle { open, inner })
            }
            HandleInner::ShutDown => Err(HandleUpgradeError::ShutDown),
        }
    }

    pub(crate) fn is_same_handle_as(&self, other: &WeakHandle<T>) -> bool {
        Weak::ptr_eq(&self.inner, &other.inner)
    }
}

impl<T: Types> std::ops::Deref for Handle<T> {
    type Target = T::Timeline;
    fn deref(&self) -> &Self::Target {
        &self.open
    }
}

impl<T: Types> Handle<T> {
    pub(crate) fn downgrade(&self) -> WeakHandle<T> {
        WeakHandle {
            inner: Arc::downgrade(&self.inner),
        }
    }
}

impl<T: Types> PerTimelineState<T> {
    /// After this method returns, [`Cache::get`] will never again return a [`Handle`]
    /// to the [`Types::Timeline`] that embeds this per-timeline state.
    /// Even if [`TenantManager::resolve`] would still resolve to it.
    ///
    /// Already-alive [`Handle`]s for will remain open, usable, and keeping the [`Types::Timeline`] alive.
    /// That's ok because they're short-lived. See module-level comment for details.
    #[instrument(level = "trace", skip_all)]
    pub(super) fn shutdown(&self) {
        let handles = self
            .handles
            .lock()
            .expect("mutex poisoned")
            // NB: this .take() sets locked to None.
            // That's what makes future `Cache::get` misses fail.
            // Cache hits are taken care of below.
            .take();
        let Some(handles) = handles else {
            trace!("already shut down");
            return;
        };
        for handle_inner_arc in handles.values() {
            // Make hits fail.
            let mut lock_guard = handle_inner_arc.lock().expect("poisoned");
            lock_guard.shutdown();
        }
        drop(handles);
    }
}

// When dropping a [`Cache`], prune its handles in the [`PerTimelineState`] to break the reference cycle.
impl<T: Types> Drop for Cache<T> {
    fn drop(&mut self) {
        for (
            _,
            WeakHandle {
                inner: handle_inner_weak,
            },
        ) in self.map.drain()
        {
            let Some(handle_inner_arc) = handle_inner_weak.upgrade() else {
                continue;
            };
            let Some(handle_timeline) = handle_inner_arc
                // locking rules: drop lock before acquiring other lock below
                .lock()
                .expect("poisoned")
                .shutdown()
            else {
                // Concurrent PerTimelineState::shutdown.
                continue;
            };
            // Clean up per_timeline_state so the HandleInner allocation can be dropped.
            let per_timeline_state = handle_timeline.per_timeline_state();
            let mut handles_lock_guard = per_timeline_state.handles.lock().expect("mutex poisoned");
            let Some(handles) = &mut *handles_lock_guard else {
                continue;
            };
            let Some(removed_handle_inner_arc) = handles.remove(&self.id) else {
                // Concurrent PerTimelineState::shutdown.
                continue;
            };
            drop(handles_lock_guard); // locking rules!
            assert!(Arc::ptr_eq(&removed_handle_inner_arc, &handle_inner_arc));
        }
    }
}

impl<T: Types> HandleInner<T> {
    fn shutdown(&mut self) -> Option<Arc<T::Timeline>> {
        match std::mem::replace(self, HandleInner::ShutDown) {
            HandleInner::Open(timeline) => Some(timeline),
            HandleInner::ShutDown => {
                // Duplicate shutdowns are possible because both Cache::drop and PerTimelineState::shutdown
                // may do it concurrently, but locking rules disallow holding per-timeline-state lock and
                // the handle lock at the same time.
                None
            }
        }
    }
}
