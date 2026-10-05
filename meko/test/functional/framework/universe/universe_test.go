package universe

import (
	"context"
	"testing"
)

type bare struct{}

func (bare) Name() string                                                { return "b" }
func (bare) Spec() Spec                                                  { return Spec{} }
func (bare) Provider() Provider                                          { return Yugabyted }
func (bare) Namespace() string                                           { return "" }
func (bare) Conn() ConnInfo                                              { return ConnInfo{} }
func (bare) ClusterAddr() (string, int)                                  { return "", 0 }
func (bare) Nodes(context.Context) ([]Node, error)                       { return nil, nil }
func (bare) SQL(context.Context, string, string, string) (string, error) { return "", nil }
func (bare) Teardown(context.Context) error                              { return nil }

type withGflags struct{ bare }

func (withGflags) SetGflags(context.Context, map[string]string, map[string]string) error { return nil }

func TestCapabilityFindsWhatAProviderImplements(t *testing.T) {
	if _, err := Capability[Gflags](withGflags{}); err != nil {
		t.Fatal(err)
	}
	_, err := Capability[XCluster](withGflags{})
	if !IsUnsupported(err) {
		t.Fatalf("err = %v, want Unsupported so a test can skip", err)
	}
	if u := err.(*Unsupported); u.Provider != Yugabyted {
		t.Errorf("provider = %s", u.Provider)
	}
}

func TestUnsupportedSurvivesWrapping(t *testing.T) {
	err := Unsupportedf(YBA, "xcluster", "no %s", "target")
	if !IsUnsupported(wrap{err}) {
		t.Error("a wrapped Unsupported was not recognised")
	}
}

type wrap struct{ error }

func (w wrap) Unwrap() error { return w.error }
