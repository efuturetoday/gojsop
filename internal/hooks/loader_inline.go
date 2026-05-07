package hooks

import (
	"context"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

// InlineLoader returns the JS source straight from spec.source.inline.
type InlineLoader struct{}

func (InlineLoader) Load(_ context.Context, src corev1alpha1.JSHookSource) ([]byte, bool, error) {
	if src.Inline == "" {
		return nil, false, nil
	}
	return []byte(src.Inline), true, nil
}
