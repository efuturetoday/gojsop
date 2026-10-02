package dispatcher

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FromMetaMapper adapts controller-runtime / apimachinery's meta.RESTMapper
// (which is what mgr.GetRESTMapper() returns) to the trimmed RESTMapper
// interface this package consumes.
func FromMetaMapper(m meta.RESTMapper) RESTMapper {
	return metaMapperAdapter{m: m}
}

type metaMapperAdapter struct{ m meta.RESTMapper }

func (a metaMapperAdapter) KindFor(gvr schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return a.m.KindFor(gvr)
}
