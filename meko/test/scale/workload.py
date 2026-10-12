"""Deterministic synthetic workload shared by the seeder and the probes.

Every property of datapack `i` - its UUID, whether it is active, how many memories and KB
chunks it holds, which topics its vectors cluster around - is a pure function of
(i, SEED). The probes recompute a datapack's vectors from the same functions instead of
reading them back, so ground truth never depends on the database under test.

Vectors are clustered, not uniform: NUM_TOPICS unit centroids shared by all datapacks,
each datapack drawing from TOPICS_PER_DATAPACK of them. Uniform random vectors in 1536
dimensions are nearly orthogonal and make ANN search look artificially hard; shared
topics also put other datapacks' vectors close to every query, which is the case a
post-filtered HNSW search gets wrong.
"""

import hashlib
import os
import uuid

import numpy as np

SEED = 20260928
DIM = 1536
NUM_TOPICS = 2000
TOPICS_PER_DATAPACK = 3
# Within-topic noise. With unit centroids and noise of norm ~NOISE, a vector's cosine to
# its centroid is ~1/sqrt(1+NOISE^2) = 0.89.
NOISE = 0.5

ACTIVE_FRACTION = 0.20
LARGE_FRACTION_OF_ACTIVE = 0.01
LARGE_MEMORIES = 5000
LARGE_CHUNKS = 5000
# Small active datapacks: lognormal sizes. Medians 50 and 20, capped.
SMALL_MEM_MEDIAN, SMALL_CHUNK_MEDIAN, SMALL_SIGMA, SMALL_CAP = 50, 20, 1.0, 2000
IDLE_MAX_MEMORIES = 5

NAMESPACE = uuid.UUID("5d1f6c9e-8a44-4b8e-9f0c-0e1a2b3c4d5e")
AGENT_ID = "meko_agent"

# Accounts: every datapack belongs to one of NUM_ACCOUNTS, drawn with lognormal
# weights so account sizes are skewed. At 1M datapacks the default gives a median
# of 15 per account, p90 192, p99 1,514 and a largest of about 18,000. Seeding past
# 1M grows every account in proportion. SCALE_ACCOUNTS=1 puts every datapack in one
# account, the layout of the first single-account runs. It must be the same for the
# seeder and every probe run against that seed.
NUM_ACCOUNTS = int(os.environ.get("SCALE_ACCOUNTS", "10000"))
# One extra account of BIG_ACCOUNT_SIZE datapacks, seeded from its own index range so it
# never overlaps the main one: seed [BIG_ACCOUNT_START, BIG_ACCOUNT_START + size).
BIG_ACCOUNT = NUM_ACCOUNTS
BIG_ACCOUNT_START = 5_000_000
BIG_ACCOUNT_SIZE = 50_000
ACCOUNT_SIGMA = 2.0


_account_cdf = None


def account_cdf() -> np.ndarray:
    global _account_cdf
    if _account_cdf is None:
        wts = np.random.default_rng([SEED, 7]).lognormal(0, ACCOUNT_SIGMA, NUM_ACCOUNTS)
        _account_cdf = np.cumsum(wts / wts.sum())
    return _account_cdf


def account_of(i: int) -> int:
    if BIG_ACCOUNT_START <= i < BIG_ACCOUNT_START + BIG_ACCOUNT_SIZE:
        return BIG_ACCOUNT
    return int(
        min(np.searchsorted(account_cdf(), _u(i, "account"), side="right"), NUM_ACCOUNTS - 1)
    )


def largest_account() -> int:
    return int(np.argmax(np.diff(np.concatenate([[0.0], account_cdf()]))))


def account_user_id(a: int) -> uuid.UUID:
    return uuid.uuid5(NAMESPACE, f"account-{a}")


def user_of(i: int) -> uuid.UUID:
    return account_user_id(account_of(i))


def datapack_id(i: int) -> uuid.UUID:
    return uuid.uuid5(NAMESPACE, f"datapack-{i}")


def _u(i: int, salt: str) -> float:
    h = hashlib.blake2b(f"{SEED}:{salt}:{i}".encode(), digest_size=8).digest()
    return int.from_bytes(h, "big") / 2**64


def size_class(i: int) -> str:
    if _u(i, "active") >= ACTIVE_FRACTION:
        return "idle"
    return "large" if _u(i, "large") < LARGE_FRACTION_OF_ACTIVE else "small"


def sizes(i: int) -> tuple[int, int]:
    """(memories, kb_chunks) for datapack i."""
    cls = size_class(i)
    if cls == "large":
        return LARGE_MEMORIES, LARGE_CHUNKS
    rng = np.random.default_rng([SEED, i, 1])
    if cls == "idle":
        return int(rng.integers(0, IDLE_MAX_MEMORIES + 1)), 0
    mem = int(min(SMALL_CAP, max(1, rng.lognormal(np.log(SMALL_MEM_MEDIAN), SMALL_SIGMA))))
    chk = int(min(SMALL_CAP, max(0, rng.lognormal(np.log(SMALL_CHUNK_MEDIAN), SMALL_SIGMA))))
    return mem, chk


_centroids = None


def centroids() -> np.ndarray:
    global _centroids
    if _centroids is None:
        c = np.random.default_rng([SEED, 0]).standard_normal((NUM_TOPICS, DIM), dtype=np.float32)
        _centroids = c / np.linalg.norm(c, axis=1, keepdims=True)
    return _centroids


def topics(i: int) -> np.ndarray:
    return np.random.default_rng([SEED, i, 2]).choice(
        NUM_TOPICS, TOPICS_PER_DATAPACK, replace=False
    )


def _clustered(rng: np.random.Generator, topic_ids: np.ndarray, n: int) -> np.ndarray:
    c = centroids()[rng.choice(topic_ids, n)]
    v = c + rng.standard_normal((n, DIM), dtype=np.float32) * (NOISE / np.sqrt(DIM))
    return (v / np.linalg.norm(v, axis=1, keepdims=True)).astype(np.float32)


def vectors(i: int, kind: str) -> np.ndarray:
    """All vectors of one kind ('mem' or 'kb') for datapack i, in insertion order."""
    mem, chk = sizes(i)
    n = mem if kind == "mem" else chk
    stream = 3 if kind == "mem" else 4
    return _clustered(np.random.default_rng([SEED, i, stream]), topics(i), n)


def queries(i: int, n: int) -> np.ndarray:
    """Probe queries for datapack i: fresh draws from its own topics, never stored."""
    return _clustered(np.random.default_rng([SEED, i, 5]), topics(i), n)


def row_id(i: int, kind: str, j: int) -> uuid.UUID:
    return uuid.uuid5(NAMESPACE, f"{kind}-{i}-{j}")
