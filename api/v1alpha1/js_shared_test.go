package v1alpha1

import (
	"reflect"
	"testing"
)

// Both CRDs must name resources and narrow objects with the same types, not
// with copies that drift apart.
//
// api-design.R10
func TestResourceRule_SharedByBothCRDs(t *testing.T) {
	rule := reflect.TypeFor[ResourceRule]()
	match := reflect.TypeFor[ObjectMatch]()

	cases := []struct {
		owner reflect.Type
		field string
		want  reflect.Type
	}{
		{reflect.TypeFor[AdmissionRule](), "ResourceRule", rule},
		{reflect.TypeFor[HookBinding](), "ResourceRule", rule},
		{reflect.TypeFor[JSAdmissionSpec](), "ObjectMatch", match},
		{reflect.TypeFor[HookBinding](), "ObjectMatch", match},
	}
	for _, tc := range cases {
		f, ok := tc.owner.FieldByName(tc.field)
		if !ok {
			t.Errorf("%s has no field %s", tc.owner.Name(), tc.field)
			continue
		}
		if !f.Anonymous {
			t.Errorf("%s.%s is not embedded, so its fields do not stay inline on the wire", tc.owner.Name(), tc.field)
		}
		if f.Type != tc.want {
			t.Errorf("%s.%s is %s, want the shared %s", tc.owner.Name(), tc.field, f.Type, tc.want)
		}
		if got := f.Tag.Get("json"); got != ",inline" {
			t.Errorf("%s.%s json tag = %q, want %q", tc.owner.Name(), tc.field, got, ",inline")
		}
	}
}

// HookEvent values are what WantsEvent compares the informer's event name
// against; a renamed constant would silently stop matching.
//
// api-design.R10
func TestHookBinding_EventDefaults(t *testing.T) {
	all := []HookEvent{HookEventAdded, HookEventModified, HookEventDeleted}

	empty := HookBinding{}
	for _, e := range all {
		if !empty.WantsEvent(e) {
			t.Errorf("an empty events list must mean all events, %q is dropped", e)
		}
	}

	only := HookBinding{Events: []HookEvent{HookEventDeleted}}
	if only.WantsEvent(HookEventAdded) || !only.WantsEvent(HookEventDeleted) {
		t.Error("only the listed event types are delivered")
	}
}
