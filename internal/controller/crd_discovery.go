package controller

import (
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

const podMonitorCRDGroupVersion = "monitoring.coreos.com/v1"

// PodMonitorCRDAvailable reports whether the Prometheus Operator PodMonitor CRD
// is registered on the cluster.
func PodMonitorCRDAvailable(cfg *rest.Config) bool {
	if cfg == nil {
		return false
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false
	}
	resources, err := dc.ServerResourcesForGroupVersion(podMonitorCRDGroupVersion)
	if err != nil {
		return false
	}
	for _, r := range resources.APIResources {
		if r.Kind == "PodMonitor" {
			return true
		}
	}
	return false
}

// ParseCommaSeparatedLabels parses "k1=v1,k2=v2" into a label map.
func ParseCommaSeparatedLabels(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, part := range splitCommaSeparated(raw) {
		k, v, ok := splitKeyValue(part)
		if !ok {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func splitCommaSeparated(raw string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == ',' {
			if i > start {
				parts = append(parts, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		parts = append(parts, raw[start:])
	}
	return parts
}

func splitKeyValue(part string) (string, string, bool) {
	for i := 0; i < len(part); i++ {
		if part[i] == '=' {
			if i == 0 || i == len(part)-1 {
				return "", "", false
			}
			return part[:i], part[i+1:], true
		}
	}
	return "", "", false
}
