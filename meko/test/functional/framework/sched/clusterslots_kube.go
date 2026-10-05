package sched

import (
	"context"
	"encoding/json"

	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// clusterSlotsConfigMap holds the entries, one data key per holder or waiter.
// It lives in `default` because every cluster has that namespace and nothing
// here may depend on a namespace some other run is tearing down.
const (
	clusterSlotsConfigMap = "ampqa-cluster-slots"
	clusterSlotsNamespace = "default"
)

type kubeSlotStore struct{ kube *k8s.Kubectl }

type slotConfigMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   slotCMMeta        `json:"metadata"`
	Data       map[string]string `json:"data,omitempty"`
}

type slotCMMeta struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

func newKubeSlotStore(kube *k8s.Kubectl) *kubeSlotStore {
	return &kubeSlotStore{kube: kube.InNamespace(clusterSlotsNamespace)}
}

func (s *kubeSlotStore) load(ctx context.Context) (map[string]slotEntry, string, error) {
	raw, err := s.kube.GetJSON(ctx, "configmap", clusterSlotsConfigMap)
	if k8s.IsNotFound(err) {
		cm, _ := json.Marshal(slotConfigMap{APIVersion: "v1", Kind: "ConfigMap",
			Metadata: slotCMMeta{Name: clusterSlotsConfigMap, Namespace: clusterSlotsNamespace}})
		if cerr := s.kube.Create(ctx, string(cm)); cerr != nil && !k8s.IsAlreadyExists(cerr) {
			return nil, "", cerr
		}
		raw, err = s.kube.GetJSON(ctx, "configmap", clusterSlotsConfigMap)
	}
	if err != nil {
		return nil, "", err
	}
	var cm slotConfigMap
	if err := json.Unmarshal([]byte(raw), &cm); err != nil {
		return nil, "", err
	}
	out := map[string]slotEntry{}
	for k, v := range cm.Data {
		var e slotEntry
		// A value this code cannot read is dropped on the next write, not obeyed.
		if json.Unmarshal([]byte(v), &e) == nil {
			out[k] = e
		}
	}
	return out, cm.Metadata.ResourceVersion, nil
}

func (s *kubeSlotStore) store(ctx context.Context, entries map[string]slotEntry, version string) (bool, error) {
	cm := slotConfigMap{APIVersion: "v1", Kind: "ConfigMap",
		Metadata: slotCMMeta{Name: clusterSlotsConfigMap, Namespace: clusterSlotsNamespace, ResourceVersion: version},
		Data:     map[string]string{}}
	for k, e := range entries {
		b, _ := json.Marshal(e)
		cm.Data[k] = string(b)
	}
	b, _ := json.Marshal(cm)
	err := s.kube.Replace(ctx, string(b))
	if k8s.IsConflict(err) {
		return true, nil
	}
	return false, err
}
