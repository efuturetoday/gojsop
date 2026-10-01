package jsregistry

import (
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// Kind names the resource kind a registry entry belongs to. Both controllers
// share one Registry and both kinds are cluster-scoped, so a bare
// NamespacedName would let a JSHook and a JSAdmission of the same name replace
// each other's VM.
type Kind string

const (
	KindJSHook      Kind = "JSHook"
	KindJSAdmission Kind = "JSAdmission"
)

// Key identifies one registry entry: the kind plus the resource name.
//
// js-registry.R14
type Key struct {
	Kind Kind
	Name types.NamespacedName
}

// HookKey is the registry key of the JSHook named name.
func HookKey(name types.NamespacedName) Key { return Key{Kind: KindJSHook, Name: name} }

// AdmissionKey is the registry key of the JSAdmission named name.
func AdmissionKey(name types.NamespacedName) Key { return Key{Kind: KindJSAdmission, Name: name} }

func (k Key) String() string { return fmt.Sprintf("%s/%s", k.Kind, k.Name) }
