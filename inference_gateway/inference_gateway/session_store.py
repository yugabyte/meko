"""In-memory session store.

Per session we keep:
  - a sliding window of the last N (role, content) message tuples
  - the MCP/Langfuse `conversation_id` minted on the first turn

The store is bounded: when we hit `max_sessions`, the least-recently-used
session is evicted. State is process-local, so a restart loses everything
and a multi-instance deploy does not share history. That's intentional for
v1 — Playground sessions are short and exploratory; if either constraint
becomes a real problem, swap this implementation for Redis without touching
`chain.py` (same interface).
"""

from __future__ import annotations

import asyncio
from collections import OrderedDict, deque
from dataclasses import dataclass, field
from typing import Deque, List, Optional, Tuple


Message = Tuple[str, str]  # (role, content)


@dataclass
class _SessionState:
    messages: Deque[Message] = field(default_factory=deque)
    conversation_id: Optional[str] = None


class InMemorySessionStore:
    """Per-session sliding window + session_id → conversation_id mapping.

    All public methods are coroutine-safe via a single lock. The lock is
    cheap because we only hold it for the short duration of dict / deque ops.
    """

    def __init__(
        self,
        max_messages_per_session: int = 10,
        max_sessions: int = 1000,
    ) -> None:
        self._max_messages = max_messages_per_session
        self._max_sessions = max_sessions
        # OrderedDict so we can do LRU eviction: most-recently-touched at the right.
        self._sessions: "OrderedDict[str, _SessionState]" = OrderedDict()
        self._lock = asyncio.Lock()

    async def append(self, session_id: str, role: str, content: str) -> None:
        """Append a message to a session's history (creates the session if new)."""
        async with self._lock:
            state = self._touch_locked(session_id)
            state.messages.append((role, content))

    async def get_messages(self, session_id: str) -> List[Message]:
        """Return a copy of the session's last ≤N messages, oldest first."""
        async with self._lock:
            state = self._sessions.get(session_id)
            if state is None:
                return []
            self._sessions.move_to_end(session_id)
            return list(state.messages)

    async def get_conversation_id(self, session_id: str) -> Optional[str]:
        """Return the MCP conversation_id minted for this session, if any."""
        async with self._lock:
            state = self._sessions.get(session_id)
            return state.conversation_id if state else None

    async def set_conversation_id(self, session_id: str, conversation_id: str) -> None:
        """Bind a session to its MCP conversation_id (called once per session)."""
        async with self._lock:
            state = self._touch_locked(session_id)
            state.conversation_id = conversation_id

    # --- internal ---

    def _touch_locked(self, session_id: str) -> _SessionState:
        """Get-or-create + bump LRU position. Caller holds the lock."""
        if session_id in self._sessions:
            self._sessions.move_to_end(session_id)
            return self._sessions[session_id]
        # New session: evict oldest until we're under the cap.
        while len(self._sessions) >= self._max_sessions:
            self._sessions.popitem(last=False)
        state = _SessionState(messages=deque(maxlen=self._max_messages))
        self._sessions[session_id] = state
        return state
