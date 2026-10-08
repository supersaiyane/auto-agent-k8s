package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Config reload, part 1 (PLAN-002 A1): which ConfigMaps and Secrets a pod
// template uses, which keys, and whether a running pod would see a change
// without a restart. Pure functions; the reloader (reload.go) acts on them.

// refKind is the kind of a referenced object.
type refKind string

const (
	refConfigMap refKind = "ConfigMap"
	refSecret    refKind = "Secret"
)

// objRef names a ConfigMap or Secret in the workload's namespace.
type objRef struct {
	Kind refKind
	Name string
}

func (r objRef) String() string { return strings.ToLower(string(r.Kind)) + "/" + r.Name }

// refUse is how a pod template uses one object.
type refUse struct {
	AllKeys bool            // envFrom, or a volume without items: every key matters
	Keys    map[string]bool // the keys named by env valueFrom or volume items
	// NeedsRestart: an env var, envFrom, or a subPath mount, which a running
	// pod never sees change. A plain volume mount is updated in place by the
	// kubelet, so it does not need a restart (reloadOn: auto).
	NeedsRestart bool
}

func (u *refUse) addKey(k string) {
	if u.Keys == nil {
		u.Keys = map[string]bool{}
	}
	u.Keys[k] = true
}

// templateRefs lists every ConfigMap and Secret a pod spec uses, through
// env valueFrom, envFrom, volumes and projected volumes, in containers and
// init containers.
func templateRefs(spec *corev1.PodSpec) map[objRef]*refUse {
	refs := map[objRef]*refUse{}
	use := func(kind refKind, name string) *refUse {
		r := objRef{kind, name}
		if refs[r] == nil {
			refs[r] = &refUse{}
		}
		return refs[r]
	}
	containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for _, c := range containers {
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if k := e.ValueFrom.ConfigMapKeyRef; k != nil {
				u := use(refConfigMap, k.Name)
				u.addKey(k.Key)
				u.NeedsRestart = true
			}
			if k := e.ValueFrom.SecretKeyRef; k != nil {
				u := use(refSecret, k.Name)
				u.addKey(k.Key)
				u.NeedsRestart = true
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.ConfigMapRef != nil {
				u := use(refConfigMap, ef.ConfigMapRef.Name)
				u.AllKeys, u.NeedsRestart = true, true
			}
			if ef.SecretRef != nil {
				u := use(refSecret, ef.SecretRef.Name)
				u.AllKeys, u.NeedsRestart = true, true
			}
		}
	}
	subPathVolumes := map[string]bool{}
	for _, c := range containers {
		for _, m := range c.VolumeMounts {
			if m.SubPath != "" || m.SubPathExpr != "" {
				subPathVolumes[m.Name] = true
			}
		}
	}
	for _, v := range spec.Volumes {
		for _, r := range volumeRefs(v) {
			u := use(r.kind, r.name)
			if len(r.keys) == 0 {
				u.AllKeys = true
			}
			for _, k := range r.keys {
				u.addKey(k)
			}
			if subPathVolumes[v.Name] {
				u.NeedsRestart = true
			}
		}
	}
	return refs
}

type volumeRef struct {
	kind refKind
	name string
	keys []string // empty: all keys
}

// volumeRefs lists the objects one volume mounts, projected sources included.
func volumeRefs(v corev1.Volume) []volumeRef {
	var out []volumeRef
	itemKeys := func(items []corev1.KeyToPath) []string {
		keys := make([]string, 0, len(items))
		for _, it := range items {
			keys = append(keys, it.Key)
		}
		return keys
	}
	if cm := v.ConfigMap; cm != nil {
		out = append(out, volumeRef{refConfigMap, cm.Name, itemKeys(cm.Items)})
	}
	if s := v.Secret; s != nil {
		out = append(out, volumeRef{refSecret, s.SecretName, itemKeys(s.Items)})
	}
	if p := v.Projected; p != nil {
		for _, src := range p.Sources {
			if cm := src.ConfigMap; cm != nil {
				out = append(out, volumeRef{refConfigMap, cm.Name, itemKeys(cm.Items)})
			}
			if s := src.Secret; s != nil {
				out = append(out, volumeRef{refSecret, s.Name, itemKeys(s.Items)})
			}
		}
	}
	return out
}

// configMapData is a ConfigMap's keys and values as bytes.
func configMapData(cm *corev1.ConfigMap) map[string][]byte {
	out := make(map[string][]byte, len(cm.Data)+len(cm.BinaryData))
	for k, v := range cm.Data {
		out[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		out[k] = v
	}
	return out
}

// changedKeys names the keys added, removed or changed between two
// versions, sorted. Only names leave this function, never values.
func changedKeys(before, after map[string][]byte) []string {
	var out []string
	for k, v := range after {
		if old, ok := before[k]; !ok || string(old) != string(v) {
			out = append(out, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// affects reports whether a change to these keys touches what the
// template uses.
func (u *refUse) affects(changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	if u.AllKeys {
		return true
	}
	for _, k := range changed {
		if u.Keys[k] {
			return true
		}
	}
	return false
}

// usedHash is a short SHA-256 over the keys a template uses from one
// object, in a fixed order. It goes into the pod template annotation; the
// values themselves are never stored or logged.
func usedHash(u *refUse, data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		if u.AllKeys || u.Keys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
