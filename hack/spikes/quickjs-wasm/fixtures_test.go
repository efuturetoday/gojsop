package quickjswasm

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// smallPolicy is config/samples/core_v1alpha1_jsadmission_validating.yaml.
const smallPolicy = `
function validate(req) {
  const obj = req.object;
  const containers = (obj.spec && obj.spec.containers) || [];
  for (const c of containers) {
    if (c.image && c.image.endsWith(":latest")) {
      return { allowed: false, message: "image " + c.image + " uses :latest, refusing" };
    }
  }
  return { allowed: true };
}
`

// largePolicySuffix runs on top of lodash (544 KB, unminified), the size of
// a policy bundled with a common library.
const largePolicySuffix = `
function validate(req) {
  const containers = _.get(req, "object.spec.containers", []);
  const bad = _.filter(containers, c => _.endsWith(c.image, ":latest"));
  if (!_.isEmpty(bad)) {
    return { allowed: false, message: _.map(bad, "image").join(", ") + " use :latest" };
  }
  return { allowed: true };
}
`

func largePolicy(tb testing.TB) string {
	tb.Helper()
	b, err := os.ReadFile("testdata/lodash.js")
	if err != nil {
		tb.Fatal(err)
	}
	return string(b) + largePolicySuffix
}

// admissionRequest is a pod CREATE request of about 4 KB.
func admissionRequest(tb testing.TB) []byte {
	tb.Helper()
	containers := make([]any, 0, 8)
	for i := range 8 {
		containers = append(containers, map[string]any{
			"name":  fmt.Sprintf("c%d", i),
			"image": fmt.Sprintf("registry.example.com/team/app-%d:1.2.%d", i, i),
			"env": []any{
				map[string]any{"name": "A", "value": "1"},
				map[string]any{"name": "B", "value": "2"},
			},
			"resources": map[string]any{
				"limits":   map[string]any{"cpu": "500m", "memory": "256Mi"},
				"requests": map[string]any{"cpu": "100m", "memory": "64Mi"},
			},
			"ports": []any{map[string]any{"containerPort": 8080 + i, "protocol": "TCP"}},
		})
	}
	req := map[string]any{
		"uid":       "705ab4f5-6393-11e8-b7cc-42010a800002",
		"kind":      map[string]any{"group": "", "version": "v1", "kind": "Pod"},
		"resource":  map[string]any{"group": "", "version": "v1", "resource": "pods"},
		"namespace": "default",
		"operation": "CREATE",
		"userInfo":  map[string]any{"username": "admin", "groups": []any{"system:authenticated"}},
		"object": map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name": "web", "namespace": "default",
				"labels":      map[string]any{"app": "web", "tier": "frontend"},
				"annotations": map[string]any{"example.com/owner": "team-a"},
			},
			"spec": map[string]any{"containers": containers, "restartPolicy": "Always"},
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}
