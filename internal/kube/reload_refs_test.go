package kube

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func cmKey(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
}

func secKey(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
}

// describe renders refs as "kind/name:keys:restart" lines, sorted.
func describe(refs map[objRef]*refUse) string {
	var lines []string
	for r, u := range refs {
		keys := "*"
		if !u.AllKeys {
			var ks []string
			for k := range u.Keys {
				ks = append(ks, k)
			}
			keys = strings.Join(sortStrings(ks), ",")
		}
		restart := "live"
		if u.NeedsRestart {
			restart = "restart"
		}
		lines = append(lines, r.String()+":"+keys+":"+restart)
	}
	return strings.Join(sortStrings(lines), " ")
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

// PLAN-002 A1.1: every way a pod spec can use a ConfigMap or Secret.
func TestTemplateRefs(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec corev1.PodSpec
		want string
	}{
		{"env valueFrom", corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{
			{Name: "A", ValueFrom: cmKey("app", "level")}, {Name: "B", ValueFrom: secKey("db", "password")}, {Name: "C", Value: "plain"}}}}},
			"configmap/app:level:restart secret/db:password:restart"},
		{"envFrom in an init container", corev1.PodSpec{InitContainers: []corev1.Container{{EnvFrom: []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "all"}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}}}}}}},
			"configmap/all:*:restart secret/creds:*:restart"},
		{"plain volumes update in place", corev1.PodSpec{Containers: []corev1.Container{{VolumeMounts: []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/app"}}}},
			Volumes: []corev1.Volume{
				{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "files"}}}},
				{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "cert", Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "c"}}}}}}},
			"configmap/files:*:live secret/cert:tls.crt:live"},
		{"subPath mounts need a restart", corev1.PodSpec{Containers: []corev1.Container{{VolumeMounts: []corev1.VolumeMount{
			{Name: "cfg", MountPath: "/etc/app.yaml", SubPath: "app.yaml"}, {Name: "exp", MountPath: "/x", SubPathExpr: "$(POD)"}}}},
			Volumes: []corev1.Volume{
				{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "files"},
					Items: []corev1.KeyToPath{{Key: "app.yaml", Path: "app.yaml"}}}}},
				{Name: "exp", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}}}},
			"configmap/files:app.yaml:restart secret/s:*:restart"},
		{"projected sources", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "pc"}, Items: []corev1.KeyToPath{{Key: "k"}}}},
			{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "ps"}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{}}}}}}}},
			"configmap/pc:k:live secret/ps:*:live"},
		{"one object used two ways merges", corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{{Name: "A", ValueFrom: cmKey("app", "a")}}}},
			Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app"},
				Items: []corev1.KeyToPath{{Key: "b"}}}}}}},
			"configmap/app:a,b:restart"},
		{"nothing used", corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}}}}, ""},
	} {
		if got := describe(templateRefs(&tc.spec)); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

// PLAN-002 A1.2: only names of changed keys leave; a used key changing is
// detected, an unused one is not; the hash covers used keys only.
func TestChangeDetection(t *testing.T) {
	before := map[string][]byte{"level": []byte("info"), "color": []byte("blue"), "gone": []byte("x")}
	after := map[string][]byte{"level": []byte("debug"), "color": []byte("blue"), "new": []byte("y")}
	if got := strings.Join(changedKeys(before, after), ","); got != "gone,level,new" {
		t.Fatalf("changed: %s", got)
	}
	level := &refUse{Keys: map[string]bool{"level": true}}
	color := &refUse{Keys: map[string]bool{"color": true}}
	all := &refUse{AllKeys: true}
	if !level.affects([]string{"level"}) || color.affects([]string{"level", "new"}) || !all.affects([]string{"new"}) || all.affects(nil) {
		t.Fatal("affects")
	}
	if usedHash(color, before) != usedHash(color, after) {
		t.Fatal("a change to an unused key leaves the hash alone")
	}
	if usedHash(level, before) == usedHash(level, after) || len(usedHash(all, after)) != 16 {
		t.Fatal("a change to a used key changes the hash")
	}
	cm := &corev1.ConfigMap{Data: map[string]string{"a": "1"}, BinaryData: map[string][]byte{"b": {2}}}
	if d := configMapData(cm); string(d["a"]) != "1" || d["b"][0] != 2 {
		t.Fatalf("configMapData: %v", d)
	}
}

// PLAN-002 A1.3: one case per annotation, Stakater's and ours.
func TestShouldReload(t *testing.T) {
	app := objRef{refConfigMap, "app"}
	db := objRef{refSecret, "db"}
	envUse := &refUse{Keys: map[string]bool{"level": true}, NeedsRestart: true}
	volUse := &refUse{AllKeys: true}
	changed := []string{"level"}
	for _, tc := range []struct {
		name   string
		ann    map[string]string
		ref    objRef
		use    *refUse
		objAnn map[string]string
		def    string
		want   bool
	}{
		{"used env key changed", nil, app, envUse, nil, reloadOnAuto, true},
		{"unused object", nil, app, nil, nil, reloadOnAuto, false},
		{"volume updates in place under auto", nil, app, volUse, nil, reloadOnAuto, false},
		{"volume restarts under always (default)", nil, app, volUse, nil, reloadOnAlways, true},
		{"volume restarts under always (annotation)", map[string]string{annReloadOn: "always"}, app, volUse, nil, reloadOnAuto, true},
		{"annotation auto overrides an always default", map[string]string{annReloadOn: "auto"}, app, volUse, nil, reloadOnAlways, false},
		{"a bad reload-on falls back to the default", map[string]string{annReloadOn: "sometimes"}, app, volUse, nil, reloadOnAlways, true},
		{"auto-agent.io/reload false", map[string]string{annReload: "false"}, app, envUse, nil, reloadOnAuto, false},
		{"reloader.stakater.com/auto false", map[string]string{annStakaterAuto: "false"}, db, envUse, nil, reloadOnAuto, false},
		{"configmap auto false spares secrets", map[string]string{annStakaterCMAuto: "false"}, db, envUse, nil, reloadOnAuto, true},
		{"configmap auto false stops configmaps", map[string]string{annStakaterCMAuto: "false"}, app, envUse, nil, reloadOnAuto, false},
		{"secret auto false", map[string]string{annStakaterSecAuto: "false"}, db, envUse, nil, reloadOnAuto, false},
		{"named by stakater reload, unused", map[string]string{annStakaterCMReload: "other, app"}, app, nil, nil, reloadOnAuto, true},
		{"named by our list", map[string]string{annReloadSecrets: "db"}, db, nil, nil, reloadOnAuto, true},
		{"named secret by stakater", map[string]string{annStakaterSecReload: "db"}, db, volUse, nil, reloadOnAuto, true},
		{"named configmap list", map[string]string{annReloadConfigMaps: "app"}, app, volUse, nil, reloadOnAuto, true},
		{"search without match", map[string]string{annStakaterSearch: "true"}, app, envUse, nil, reloadOnAuto, false},
		{"search with match", map[string]string{annStakaterSearch: "true"}, app, envUse, map[string]string{annStakaterMatch: "true"}, reloadOnAuto, true},
		{"object ignored (stakater)", nil, app, envUse, map[string]string{annStakaterIgnore: "true"}, reloadOnAuto, false},
		{"object ignored (ours), even when named", map[string]string{annReloadConfigMaps: "app"}, app, envUse, map[string]string{annReload: "false"}, reloadOnAuto, false},
	} {
		if got := shouldReload(workloadRules(tc.ann), tc.ref, tc.use, changed, tc.objAnn, tc.def); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if shouldReload(workloadRules(nil), app, envUse, nil, nil, reloadOnAuto) {
		t.Fatal("no changed key, no reload")
	}
}
