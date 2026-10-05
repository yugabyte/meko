package ampstack

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
)

// BackingAddress selects how the controller and endpoints address a co-located
// backing. Pods hands them every tserver's stable pod DNS name. Service hands
// them one ClusterIP Service over all tservers, publishing YSQL and tserver RPC,
// which is what the operator foundation gives customers.
type BackingAddress struct {
	Mode string
	// SessionAffinity is the Service's spec.sessionAffinity: None or ClientIP.
	SessionAffinity string
}

const (
	BackingAddressPods    = "pods"
	BackingAddressService = "service"

	tserverRPCPort = 9100

	tserverServiceStackLabel = "ampqa-stack"
)

// The tserver selectors a ybk8s deploy and a YBA kubernetes universe stamp.
var tserverServiceSelectors = []struct{ key, value string }{
	{"app.kubernetes.io/name", "yb-tserver"},
	{"app", "yb-tserver"},
}

func (a *App) serviceAddressed() bool {
	return a.externalBacking() && a.cfg.BackingAddress.Mode == BackingAddressService
}

// TserverServiceName is unique per control plane, so two stacks on one backing
// never share or delete each other's Service.
func (a *App) TserverServiceName() string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(a.cfg.Namespace))
	return fmt.Sprintf("ampqa-tserver-conn-%08x", h.Sum32())
}

func (a *App) controllerHostPorts() string {
	if !a.serviceAddressed() {
		return a.cfg.Backing.HostPorts
	}
	_, port := firstHostPort(a.cfg.Backing.HostPorts)
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", a.TserverServiceName(), strings.TrimSpace(a.cfg.Backing.KubeNamespace), port)
}

func (a *App) ensureTserverService(ctx context.Context) error {
	switch a.cfg.BackingAddress.Mode {
	case "", BackingAddressPods:
		return nil
	case BackingAddressService:
	default:
		return fmt.Errorf("ampstack: backing_address.mode %q: want %q or %q", a.cfg.BackingAddress.Mode, BackingAddressPods, BackingAddressService)
	}
	affinity := orDefault(a.cfg.BackingAddress.SessionAffinity, "None")
	if affinity != "None" && affinity != "ClientIP" {
		return fmt.Errorf("ampstack: backing_address.session_affinity %q: want None or ClientIP", affinity)
	}
	ns := strings.TrimSpace(a.cfg.Backing.KubeNamespace)
	if ns == "" {
		return fmt.Errorf("ampstack: backing_address.mode=service needs a backing in this cluster (a ybk8s deploy or a YBA kubernetes universe); %s names no namespace", a.cfg.Backing.HostPorts)
	}
	// The node certificates name the pods, not this Service; the foundation adds
	// its Service through tls.extraSANs, which nothing here can.
	if a.cfg.TLS.Enabled || strings.HasPrefix(a.cfg.Backing.SSLMode, "verify") {
		return fmt.Errorf("ampstack: backing_address.mode=service cannot verify TLS: the tserver certificates do not name the Service")
	}
	kube := a.kube.InNamespace(ns)
	for _, sel := range tserverServiceSelectors {
		names, err := kube.GetSelector(ctx, "pod", sel.key+"="+sel.value, "{.items[*].metadata.name}")
		if err != nil {
			return fmt.Errorf("ampstack: list tserver pods in %s: %w", ns, err)
		}
		if len(strings.Fields(names)) == 0 {
			continue
		}
		_, ysql := firstHostPort(a.cfg.Backing.HostPorts)
		if err := kube.Apply(ctx, tserverServiceManifest(a.TserverServiceName(), ns, a.cfg.Namespace, sel.key, sel.value, affinity, ysql)); err != nil {
			return fmt.Errorf("ampstack: apply tserver Service in %s: %w", ns, err)
		}
		return nil
	}
	return fmt.Errorf("ampstack: no tserver pod in %s matches app.kubernetes.io/name=yb-tserver or app=yb-tserver", ns)
}

func tserverServiceManifest(name, namespace, stack, selKey, selValue, affinity string, ysqlPort int) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
    %s: %s
spec:
  type: ClusterIP
  sessionAffinity: %s
  selector:
    %s: %s
  ports:
    - name: tcp-ysql-port
      port: %d
      targetPort: %d
    - name: tcp-rpc-port
      port: %d
      targetPort: %d
`, name, namespace, ManagedByLabel, ManagedByValue, tserverServiceStackLabel, stack, affinity, selKey, selValue, ysqlPort, ysqlPort, tserverRPCPort, tserverRPCPort)
}
