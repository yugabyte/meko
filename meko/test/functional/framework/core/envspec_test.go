package core

import "testing"

func TestApplyStackOverride(t *testing.T) {
	mk := func() []Test {
		return []Test{
			{Name: "a", Env: EnvSpec{Shape: SingleStack}},
			{Name: "b", Env: EnvSpec{Shape: DedicatedStack}},
			{Name: "c", Env: EnvSpec{Shape: Remote}},
		}
	}
	cases := []struct {
		value string
		want  []Shape
	}{
		{"", []Shape{SingleStack, DedicatedStack, Remote}},
		{"shared", []Shape{SingleStack, SingleStack, Remote}},
		{"dedicated", []Shape{DedicatedStack, DedicatedStack, Remote}},
	}
	for _, c := range cases {
		tests := mk()
		cfg := NewConfig()
		cfg.Set(StackOverrideKey, c.value)
		if err := ApplyStackOverride(tests, cfg); err != nil {
			t.Fatalf("%q: %v", c.value, err)
		}
		for i, tc := range tests {
			if tc.Env.Shape != c.want[i] {
				t.Errorf("%q: %s shape = %s, want %s", c.value, tc.Name, tc.Env.Shape, c.want[i])
			}
		}
	}
	bogus := NewConfig()
	bogus.Set(StackOverrideKey, "bogus")
	if err := ApplyStackOverride(mk(), bogus); err == nil {
		t.Error("bogus value must be rejected")
	}
	if !(EnvSpec{Shape: DedicatedStack, FaultsSharedInfra: true}).Dedicated() ||
		(EnvSpec{Shape: DedicatedStack, FaultsSharedInfra: true}).FaultsShared() ||
		!(EnvSpec{Shape: SingleStack, FaultsSharedInfra: true}).FaultsShared() {
		t.Error("FaultsShared must be off on a dedicated stack and on for a shared one")
	}
}
