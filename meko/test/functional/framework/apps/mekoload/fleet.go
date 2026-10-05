package mekoload

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

const (
	remoteBinary = "/tmp/meko-load"
	remoteGo     = "/tmp/meko-load.go"
)

// podHandle is the client pod a Fleet launches. *clientpod.K8s is the real one.
type podHandle interface {
	clientpod.ClientPod
	Exec() exec.RemoteExecutor
}

// Pod is one client pod running meko-load.
type Pod struct {
	Name   string
	Client *Client
	handle podHandle
	pf     *k8s.PortForward

	targetID string
}

// Fleet is the client pods of one test.
type Fleet struct {
	cfg  Config
	kube *k8s.Kubectl
	logf func(string, ...any)

	resolve func(ctx context.Context) (Origin, error)
	newPod  func(kube *k8s.Kubectl, readyTimeout time.Duration) podHandle
	dial    func(ctx context.Context, addr string) error
	// control returns the base URL of a pod's control API.
	control func(ctx context.Context, p *Pod) (string, error)
	poll    time.Duration

	mu     sync.Mutex
	pods   []*Pod
	origin Origin
}

// New binds a fleet to the namespace kube points at, which is where the pods run.
func New(cfg Config, kube *k8s.Kubectl, logf func(string, ...any)) *Fleet {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	f := &Fleet{
		cfg: cfg, kube: kube, logf: logf, poll: 2 * time.Second,
		resolve: NewFetcher(cfg, logf).Resolve,
		newPod: func(kube *k8s.Kubectl, ready time.Duration) podHandle {
			p := clientpod.NewK8s(kube)
			p.ReadyTimeout = ready
			return p
		},
		dial: dialTCP,
	}
	f.control = f.reachPod
	return f
}

func dialTCP(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// Origin is where Start got the binary.
func (f *Fleet) Origin() Origin { return f.origin }

func (f *Fleet) Pods() []*Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Pod(nil), f.pods...)
}

func (f *Fleet) Len() int { return len(f.Pods()) }

// Start launches n pods named <prefix>-<i> from a public base image, copies the
// binary into each, and waits until every control API answers. The pods that
// did start stay registered when it fails, so Close removes them.
func (f *Fleet) Start(ctx context.Context, prefix string, n int) error {
	if n < 1 {
		return fmt.Errorf("a fleet needs at least one pod, got %d", n)
	}
	origin, err := f.resolve(ctx)
	if err != nil {
		return err
	}
	f.origin = origin
	f.logf("mekoload: binary from %s %s", origin.Kind, origin.Ref)

	pods := make([]*Pod, n)
	for i := range pods {
		pods[i] = &Pod{Name: fmt.Sprintf("%s-%d", prefix, i)}
	}
	f.mu.Lock()
	f.pods = append(f.pods, pods...)
	f.mu.Unlock()

	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, p := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.startPod(ctx, p, origin.Path)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (f *Fleet) startPod(ctx context.Context, p *Pod, binary string) error {
	p.handle = f.newPod(f.kube, f.cfg.ReadyTimeout)
	spec := clientpod.Spec{
		Name:  p.Name,
		Image: f.cfg.Image,
		Command: []string{"sh", "-c", fmt.Sprintf(
			"while [ ! -f %s ]; do sleep 1; done; exec %s --port %d --log-json", remoteGo, remoteBinary, f.cfg.Port)},
		CPU:    f.cfg.CPU,
		Memory: f.cfg.Memory,
	}
	if err := p.handle.Start(ctx, spec); err != nil {
		return fmt.Errorf("start %s: %w", p.Name, err)
	}
	ex := p.handle.Exec()
	if err := ex.Copy(ctx, binary, remoteBinary); err != nil {
		return fmt.Errorf("copy the binary into %s: %w", p.Name, err)
	}
	res, err := ex.Run(ctx, fmt.Sprintf("chmod 755 %s && touch %s", remoteBinary, remoteGo))
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("release the binary in %s: %v %s", p.Name, err, strings.TrimSpace(res.Stderr))
	}
	base, err := f.control(ctx, p)
	if err != nil {
		return fmt.Errorf("reach %s: %w", p.Name, err)
	}
	p.Client = NewClient(base)
	deadline := time.Now().Add(f.cfg.ReadyTimeout)
	for {
		if h, herr := p.Client.Health(ctx); herr == nil && h.Status == "ok" {
			f.logf("mekoload: %s is up at %s (%s)", p.Name, base, h.Version)
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("%s did not answer health within %s: %v%s", p.Name, f.cfg.ReadyTimeout, herr, f.podLog(ctx, p))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.poll):
		}
	}
}

func (f *Fleet) podLog(ctx context.Context, p *Pod) string {
	var lines []string
	_ = p.handle.Follow(ctx, func(l string) { lines = append(lines, l) })
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	if len(lines) == 0 {
		return ""
	}
	return "\npod log tail:\n" + strings.Join(lines, "\n")
}

func (f *Fleet) reachPod(ctx context.Context, p *Pod) (string, error) {
	port := f.cfg.Port
	if f.cfg.Reach != "port_forward" {
		if ip, err := f.kube.Get(ctx, "pod", p.Name, "{.status.podIP}"); err == nil && ip != "" {
			addr := net.JoinHostPort(ip, fmt.Sprint(port))
			// The binary was released a moment ago and may not be listening yet.
			for i := 0; i < 3; i++ {
				if f.dial(ctx, addr) == nil {
					return "http://" + addr, nil
				}
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(f.poll):
				}
			}
		}
	}
	pf, err := f.kube.PortForward(ctx, "pod/"+p.Name, 0, port)
	if err != nil {
		return "", err
	}
	p.pf = pf
	return "http://" + pf.LocalAddr(), nil
}

// CreateTargets gives every pod a target built by spec(i).
func (f *Fleet) CreateTargets(ctx context.Context, spec func(i int) TargetSpec) error {
	pods := f.Pods()
	errs := make([]error, len(pods))
	ids := make([]string, len(pods))
	var wg sync.WaitGroup
	for i, p := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = p.Client.CreateTarget(ctx, spec(i))
		}()
	}
	wg.Wait()
	f.mu.Lock()
	for i, p := range pods {
		p.targetID = ids[i]
	}
	f.mu.Unlock()
	return errors.Join(errs...)
}

// Close collects the pods' logs into sink and deletes them. It runs to the end
// whatever happens, and reports what failed.
func (f *Fleet) Close(ctx context.Context, sink apps.ArtifactSink) error {
	f.mu.Lock()
	pods := f.pods
	f.pods = nil
	f.mu.Unlock()
	var errs []error
	for _, p := range pods {
		if p.pf != nil {
			_ = p.pf.Close()
		}
		if p.handle == nil {
			continue
		}
		if err := p.handle.Stop(ctx, sink); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", p.Name, err))
		}
	}
	return errors.Join(errs...)
}
