package config

import "github.com/wklken/apisix-go/pkg/json"

const apisixDeploymentEtcdHostEnv = "APISIX_DEPLOYMENT_ETCD_HOST"

// applyReservedEnvironment mirrors the single field-level environment
// override provided by APISIX 3.17. Invalid JSON is ignored by APISIX and
// therefore leaves the file value unchanged here as well.
func applyReservedEnvironment(root *valueNode, environment map[string]string) error {
	raw, ok := environment[apisixDeploymentEtcdHostEnv]
	if !ok {
		return nil
	}
	var hosts []string
	if err := json.Unmarshal([]byte(raw), &hosts); err != nil {
		return nil
	}
	if hosts == nil {
		return nil
	}
	deployment := root.mapping["deployment"]
	if deployment == nil || deployment.kind != nodeMapping {
		return nil
	}
	etcd := deployment.mapping["etcd"]
	if etcd == nil || etcd.kind != nodeMapping {
		return nil
	}
	hostNode, err := nodeFromAny(hosts, "")
	if err != nil {
		return err
	}
	etcd.mapping["host"] = hostNode
	return nil
}
