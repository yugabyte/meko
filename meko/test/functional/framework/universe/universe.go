package universe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// ConnInfo is how to reach a universe's YSQL.
type ConnInfo struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// Node is one process host of a universe.
type Node struct {
	Name    string
	Cluster ClusterType
	Zone    string
	Master  bool
	Tserver bool
	Ready   bool
}

// Universe is a running YugabyteDB universe.
type Universe interface {
	Name() string
	Spec() Spec
	Provider() Provider
	// Namespace is where the universe runs, when that is a kube namespace.
	Namespace() string
	// Conn reaches the universe from the machine running the framework.
	Conn() ConnInfo
	// ClusterAddr is the host and port the universe is reached at from inside
	// the cluster, a name that survives its pods being replaced.
	ClusterAddr() (host string, port int)
	Nodes(ctx context.Context) ([]Node, error)
	// SQL runs one statement on node ("" for any) against database ("" for the
	// default) and returns the unaligned output.
	SQL(ctx context.Context, node, database, statement string) (string, error)
	Teardown(ctx context.Context) error
}

// Unsupported is returned when a provider cannot do something a test asked for,
// so the test can skip with the reason.
type Unsupported struct {
	Provider   Provider
	Capability string
	Why        string
}

func (e *Unsupported) Error() string {
	return fmt.Sprintf("%s does not support %s: %s", e.Provider, e.Capability, e.Why)
}

// Unsupportedf builds an Unsupported.
func Unsupportedf(p Provider, capability, format string, args ...any) error {
	return &Unsupported{Provider: p, Capability: capability, Why: fmt.Sprintf(format, args...)}
}

// IsUnsupported reports whether err says a provider lacks a capability.
func IsUnsupported(err error) bool {
	var u *Unsupported
	return errors.As(err, &u)
}

// Capability returns u as T, or an Unsupported error when its provider does not
// implement T at all.
func Capability[T any](u Universe) (T, error) {
	if c, ok := u.(T); ok {
		return c, nil
	}
	var zero T
	return zero, Unsupportedf(u.Provider(), reflect.TypeFor[T]().Name(), "the provider does not implement it")
}

// XClusterStatus is the state of replication from a source to a target.
type XClusterStatus struct {
	Healthy bool
	// Lag is the largest replication lag, zero when unknown.
	Lag    time.Duration
	Detail string
}

// XCluster sets up replication from this universe to another.
type XCluster interface {
	SetupXCluster(ctx context.Context, target Universe, databases []string) error
	XClusterStatus(ctx context.Context, target Universe) (XClusterStatus, error)
	TeardownXCluster(ctx context.Context, target Universe) error
}

// XClusterBootstrap is a source whose xCluster setup copies its databases to
// the target through a backup, kept in the given store.
type XClusterBootstrap interface {
	BootstrapXClusterVia(store S3)
}

// ReadReplicas adds and removes a read replica cluster.
type ReadReplicas interface {
	AddReadReplica(ctx context.Context, c ClusterSpec) error
	RemoveReadReplica(ctx context.Context) error
}

// S3 is an object store a backup goes to.
type S3 struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	Region    string
}

// Backup is a taken backup, opaque but for where it is.
type Backup struct {
	ID       string
	Database string
	Location string
	At       time.Time
}

// Backups backs a database up and restores it, here or in another universe.
type Backups interface {
	Backup(ctx context.Context, database string, to S3) (Backup, error)
	Restore(ctx context.Context, b Backup, from S3) error
}

// PITR recovers a database to a past moment.
type PITR interface {
	EnablePITR(ctx context.Context, database string, retention time.Duration) error
	RecoverTo(ctx context.Context, database string, at time.Time) error
}

// Upgrades does a rolling upgrade to another version or image.
type Upgrades interface {
	Upgrade(ctx context.Context, imageOrVersion string) error
}

// Gflags changes server flags with a rolling restart.
type Gflags interface {
	SetGflags(ctx context.Context, master, tserver map[string]string) error
}

// NodeControl stops, starts and kills one node, for fault tests.
type NodeControl interface {
	StopNode(ctx context.Context, node string) error
	StartNode(ctx context.Context, node string) error
	KillNode(ctx context.Context, node string) error
}
