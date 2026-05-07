// Package hooks resolves the source of a JSHook (inline / ConfigMap / OCI)
// to the raw JS module bytes that the runtime engine evaluates.
package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

// Loader returns the JS module text for a given JSSource.
// Implementations are wired together via Chain.
type Loader interface {
	Load(ctx context.Context, src corev1alpha1.JSSource) ([]byte, bool, error)
}

// Chain dispatches to the first loader that claims the source (returns true).
// MVP wires only the inline loader; ConfigMap and OCI come later.
type Chain struct {
	loaders []Loader
}

func NewChain(loaders ...Loader) *Chain { return &Chain{loaders: loaders} }

func (c *Chain) Load(ctx context.Context, src corev1alpha1.JSSource) ([]byte, error) {
	for _, l := range c.loaders {
		body, ok, err := l.Load(ctx, src)
		if err != nil {
			return nil, err
		}
		if ok {
			return body, nil
		}
	}
	return nil, fmt.Errorf("no loader matched source: must set one of spec.source.inline, configMapRef, or ociRef")
}

// Hash returns a stable sha256 of the JS source. Used as the instance restart
// trigger in the controller (status.instance.sourceHash).
func Hash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
