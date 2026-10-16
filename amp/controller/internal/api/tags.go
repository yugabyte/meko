package api

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// maxTags caps how many tags one cluster or endpoint carries (the Tags schema's
// maxProperties).
const maxTags = 50

// labelNameRE is a Kubernetes label name segment and, with "" also allowed, a
// label value: alphanumeric at both ends, `-`, `_` and `.` between. The
// controller has no k8s.io dependency, so this mirrors apimachinery's
// IsQualifiedName / IsValidLabelValue rather than calling them.
var labelNameRE = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// dns1123SubdomainRE is a label key's optional prefix.
var dns1123SubdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// reservedTagKeys are the labels the endpoint chart sets on every object, the
// selector pair among them: the controller finds an endpoint's pod by
// app.kubernetes.io/instance. Keep in step with the omit list in the chart's
// yb-amp-endpoint.commonLabels helper.
var reservedTagKeys = map[string]bool{
	"helm.sh/chart":                true,
	"app.kubernetes.io/name":       true,
	"app.kubernetes.io/instance":   true,
	"app.kubernetes.io/version":    true,
	"app.kubernetes.io/managed-by": true,
}

// reservedTagDomain is the controller's own label namespace: the identity labels
// (deploy.IdentityLabels) and ingress routing — the shard proxy selects ingress
// objects by amp.yugabyte.com/ingress-shard. The whole domain is refused, not
// only the keys in use today.
const reservedTagDomain = "amp.yugabyte.com"

// validateTags checks tags against the Kubernetes label rules, since they are
// rendered as labels: a tag the API server would refuse would otherwise fail the
// endpoint's deploy long after the 202. Keys are checked in sorted order so the
// error is deterministic.
func validateTags(tags map[string]string) error {
	if len(tags) > maxTags {
		return fmt.Errorf("tags: at most %d are allowed, got %d", maxTags, len(tags))
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := validateTagKey(k); err != nil {
			return fmt.Errorf("tags: key %q: %w", k, err)
		}
		if v := tags[k]; len(v) > 63 || (v != "" && !labelNameRE.MatchString(v)) {
			return fmt.Errorf("tags: value %q for key %q must be at most 63 characters of alphanumerics, '-', '_' or '.', starting and ending with an alphanumeric", v, k)
		}
	}
	return nil
}

// validateTagKey checks one key.
func validateTagKey(k string) error {
	if reservedTagKeys[k] {
		return fmt.Errorf("the endpoint chart sets this label itself")
	}
	if prefix, _, ok := strings.Cut(k, "/"); ok &&
		(prefix == reservedTagDomain || strings.HasSuffix(prefix, "."+reservedTagDomain)) {
		return fmt.Errorf("the %s prefix is reserved", reservedTagDomain)
	}
	return validateQualifiedName(k)
}

// validateQualifiedName checks a Kubernetes qualified name — a label key, taint
// key or topology key: an optional DNS-subdomain prefix and a 63-character name.
func validateQualifiedName(k string) error {
	name := k
	if prefix, rest, ok := strings.Cut(k, "/"); ok {
		if len(prefix) > 253 || !dns1123SubdomainRE.MatchString(prefix) {
			return fmt.Errorf("prefix must be a lowercase DNS subdomain of at most 253 characters")
		}
		name = rest
	}
	if len(name) > 63 || !labelNameRE.MatchString(name) {
		return fmt.Errorf("name must be 1-63 characters of alphanumerics, '-', '_' or '.', starting and ending with an alphanumeric")
	}
	return nil
}

// validateLabelValue checks a Kubernetes label value, where "" is allowed.
func validateLabelValue(v string) error {
	if len(v) > 63 || (v != "" && !labelNameRE.MatchString(v)) {
		return fmt.Errorf("value %q must be at most 63 characters of alphanumerics, '-', '_' or '.', starting and ending with an alphanumeric", v)
	}
	return nil
}

// tagsFrom unwraps an optional request field.
func tagsFrom(t *Tags) map[string]string {
	if t == nil || len(*t) == 0 {
		return nil
	}
	return *t
}

// tagsToAPI renders stored tags for a response, where the field is required and
// so is `{}` rather than null when there are none.
func tagsToAPI(t map[string]string) Tags {
	if t == nil {
		return Tags{}
	}
	return t
}
