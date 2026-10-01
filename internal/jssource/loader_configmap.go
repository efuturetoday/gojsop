package jssource

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

// DefaultConfigMapKey is the key used when ConfigMapRef.Key is unset. The CRD
// defaults this server-side too (`hook.js`); duplicating the default here
// keeps the loader robust if a v1alpha2 ever drops the default-tag.
const DefaultConfigMapKey = "hook.js"

// ConfigMapLoader resolves spec.source.configMapRef against the cluster.
// The reader is typically the manager's cache-backed client so reads are
// hot and changes are observed via the watch installed by EnqueueOnConfigMaps.
//
// The loader is cluster-scoped: ConfigMapRef carries an explicit Namespace
// because both JSHook and JSAdmission are cluster-scoped CRs and there is
// no "same namespace" fallback to rely on.
type ConfigMapLoader struct {
	Reader client.Reader
}

// Load implements Loader. Returns (nil,false,nil) when ConfigMapRef is unset
// so Chain falls through to the next loader. A configured-but-broken ref
// (missing CM, missing key, key value empty) returns an error so the
// reconciler can surface SourceLoadFailed.
// js-sources.R2
func (l ConfigMapLoader) Load(ctx context.Context, src corev1alpha1.JSSource) ([]byte, bool, error) {
	ref := src.ConfigMapRef
	if ref == nil {
		return nil, false, nil
	}
	if l.Reader == nil {
		return nil, true, fmt.Errorf("configMapRef loader: no client configured")
	}
	if ref.Name == "" || ref.Namespace == "" {
		return nil, true, fmt.Errorf("configMapRef: name and namespace are required")
	}
	key := ref.Key
	if key == "" {
		key = DefaultConfigMapKey
	}

	var cm corev1.ConfigMap
	if err := l.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, fmt.Errorf("configMapRef: ConfigMap %s/%s not found", ref.Namespace, ref.Name)
		}
		return nil, true, fmt.Errorf("configMapRef: get ConfigMap %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	// Try Data first (text); fall back to BinaryData. We never accept both —
	// kube validates that for us, but if it ever sneaks through we prefer the
	// human-readable Data.
	if v, ok := cm.Data[key]; ok {
		if v == "" {
			return nil, true, fmt.Errorf("configMapRef: %s/%s key %q is empty", ref.Namespace, ref.Name, key)
		}
		return []byte(v), true, nil
	}
	if v, ok := cm.BinaryData[key]; ok {
		if len(v) == 0 {
			return nil, true, fmt.Errorf("configMapRef: %s/%s binary key %q is empty", ref.Namespace, ref.Name, key)
		}
		return v, true, nil
	}
	return nil, true, fmt.Errorf("configMapRef: %s/%s has no key %q", ref.Namespace, ref.Name, key)
}
