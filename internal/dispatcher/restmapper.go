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

func (a metaMapperAdapter) RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	rm, err := a.m.RESTMapping(gk, versions...)
	if err != nil {
		return nil, err
	}
	return &RESTMapping{Resource: rm.Resource}, nil
}
