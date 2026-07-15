package core

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"hash/fnv"
	"math/rand/v2"
)

// Seed is a deterministic source of randomness. One run seed is chosen once
// (from --seed or crypto/rand) and every sub-seed derives from it by a stable
// hash, so the same run seed reproduces the same sequences everywhere.
type Seed struct {
	value int64
	label string
}

// NewRunSeed returns the top-level seed. If explicit is set, value is used;
// otherwise a value is drawn once from crypto/rand.
func NewRunSeed(value int64, explicit bool) *Seed {
	if !explicit {
		var b [8]byte
		_, _ = cryptorand.Read(b[:])
		value = int64(binary.LittleEndian.Uint64(b[:]))
	}
	return &Seed{value: value, label: "run"}
}

// Value is the underlying int64 (logged and recorded into results).
func (s *Seed) Value() int64 { return s.value }

// Label is a human-readable trail of where this seed came from.
func (s *Seed) Label() string { return s.label }

// ForTest derives a stable per-test seed from the run seed and the test name.
func (s *Seed) ForTest(name string) *Seed {
	return &Seed{value: mix(s.value, name), label: "test:" + name}
}

// Fork derives a stable sub-seed for a named component (e.g. "nemesis").
func (s *Seed) Fork(label string) *Seed {
	return &Seed{value: mix(s.value, label), label: s.label + "/" + label}
}

// Rand returns a fresh math/rand/v2 generator seeded from this seed. Two calls
// on the same Seed produce identical sequences.
func (s *Seed) Rand() *rand.Rand {
	hi := uint64(s.value)
	lo := uint64(s.value)>>1 ^ 0x9e3779b97f4a7c15
	return rand.New(rand.NewPCG(hi, lo))
}

func mix(seed int64, label string) int64 {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(seed))
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(label))
	return int64(h.Sum64())
}
