package jshook

// Config mirrors the shape returned by a hook's `config()` JS export.
// Fields are intentionally permissive — the controller validates what it
// actually understands, unknown fields are ignored.
//
// Schema follows shell-operator conventions so authors familiar with
// flant/shell-operator have the same mental model:
//
//	function config() {
//	  return {
//	    configVersion: "v1",
//	    onStartup: 10,
//	    schedule:    [{ name, crontab, allowFailure }],
//	    kubernetes:  [{ name, apiVersion, kind, executeHookOnEvent, ... }],
//	  };
//	}
type Config struct {
	ConfigVersion string              `json:"configVersion,omitempty"`
	OnStartup     int                 `json:"onStartup,omitempty"`
	Schedule      []ScheduleBinding   `json:"schedule,omitempty"`
	Kubernetes    []KubernetesBinding `json:"kubernetes,omitempty"`
}

type ScheduleBinding struct {
	Name         string `json:"name,omitempty"`
	Crontab      string `json:"crontab,omitempty"`
	AllowFailure bool   `json:"allowFailure,omitempty"`
	Queue        string `json:"queue,omitempty"`
}

type KubernetesBinding struct {
	Name                         string         `json:"name,omitempty"`
	APIVersion                   string         `json:"apiVersion,omitempty"`
	Kind                         string         `json:"kind,omitempty"`
	ExecuteHookOnEvent           []string       `json:"executeHookOnEvent,omitempty"`
	ExecuteHookOnSynchronization *bool          `json:"executeHookOnSynchronization,omitempty"`
	NameSelector                 *NameSelector  `json:"nameSelector,omitempty"`
	LabelSelector                map[string]any `json:"labelSelector,omitempty"`
	FieldSelector                map[string]any `json:"fieldSelector,omitempty"`
	Namespace                    *NamespaceSel  `json:"namespace,omitempty"`
	JQFilter                     string         `json:"jqFilter,omitempty"`
	AllowFailure                 bool           `json:"allowFailure,omitempty"`
	Queue                        string         `json:"queue,omitempty"`
}

type NameSelector struct {
	MatchNames []string `json:"matchNames,omitempty"`
}

type NamespaceSel struct {
	NameSelector  *NameSelector  `json:"nameSelector,omitempty"`
	LabelSelector map[string]any `json:"labelSelector,omitempty"`
}
