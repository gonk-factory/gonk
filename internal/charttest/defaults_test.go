//go:build chart

package charttest

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE CHART'S DEFAULTS NAME NO SITE.
//
// values.yaml used to ship one cluster's registry, pull Secret, GitLab URL, CA
// ConfigMap, ingress host and certificate issuer as defaults. A public chart
// that does that points every other installation at hosts it does not own, and
// the installation it was written for depends, invisibly, on a default.
//
// So: the registry default is the public one, and every site-local value is
// EMPTY -- required where the chart cannot work without it (gitlab.url, the
// ingress host when ingress is on), and simply off where it can (the private
// CA, the pull secret, the ingress annotations). These tests pin the defaults
// as data, and then pin what the render does with them.

// chartDefaults is chart/gonk/values.yaml, parsed.
func chartDefaults(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(SourceChartDir(), "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// at walks a parsed YAML document; a missing key is a test failure, so a
// renamed value cannot turn an assertion into a comparison with nil.
func at(t *testing.T, v map[string]any, path ...string) any {
	t.Helper()
	var cur any = v
	for i, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("values.yaml: %s is not a map", strings.Join(path[:i], "."))
		}
		cur, ok = m[k]
		if !ok {
			t.Fatalf("values.yaml has no key %s", strings.Join(path[:i+1], "."))
		}
	}
	return cur
}

// publicRegistry is where the release workflow publishes the four images.
const publicRegistry = "ghcr.io/gonk-factory/gonk"

// defaultCAMountPath is where a configured private CA is mounted. It names the
// product, not a site.
const defaultCAMountPath = "/etc/ssl/gonk/ca.crt"

func TestChartDefaultsNameNoSite(t *testing.T) {
	v := chartDefaults(t)
	for _, c := range []struct {
		path []string
		want any
	}{
		{[]string{"image", "registry"}, publicRegistry},
		{[]string{"gitlab", "url"}, ""},
		{[]string{"gitlab", "caCert", "existingConfigMap"}, ""},
		{[]string{"gitlab", "caCert", "key"}, ""},
		{[]string{"gitlab", "caCert", "mountPath"}, defaultCAMountPath},
		{[]string{"ingress", "host"}, ""},
		{[]string{"ingress", "enabled"}, false},
		{[]string{"intake", "webhookPublicURL"}, ""},
		{[]string{"litellm", "externalURL"}, ""},
	} {
		if got := at(t, v, c.path...); got != c.want {
			t.Errorf("values.yaml %s = %#v, want %#v", strings.Join(c.path, "."), got, c.want)
		}
	}
	if got, ok := at(t, v, "image", "pullSecrets").([]any); !ok || len(got) != 0 {
		t.Errorf("values.yaml image.pullSecrets = %#v, want an empty list: a public registry needs none, and a named Secret is a site's", at(t, v, "image", "pullSecrets"))
	}
	if got, ok := at(t, v, "ingress", "annotations").(map[string]any); !ok || len(got) != 0 {
		t.Errorf("values.yaml ingress.annotations = %#v, want an empty map: an issuer and a DNS name are site-local", at(t, v, "ingress", "annotations"))
	}
	if got, ok := at(t, v, "networkPolicy", "gitlab", "cidrs").([]any); !ok || len(got) != 0 {
		t.Errorf("values.yaml networkPolicy.gitlab.cidrs = %#v, want an empty list", at(t, v, "networkPolicy", "gitlab", "cidrs"))
	}
}

// THE SELF-BLOCK DEFAULT IS UNCHANGED, AND STILL A DEFAULT. gonk must never
// work on its own repository; the chart ships that project path so that an
// installation which says nothing is still protected. (The reference cluster
// also pins it explicitly in its HelmRelease, so it does not depend on this.)
func TestSelfBlockDefaultIsStillShipped(t *testing.T) {
	got, _ := at(t, chartDefaults(t), "intake", "blockedProjects").([]any)
	if len(got) != 1 || got[0] != "agentic/gonk-project" {
		t.Fatalf("values.yaml intake.blockedProjects = %#v, want [agentic/gonk-project]", got)
	}
	env := workloadEnv(t, MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake"))
	if env["GONK_BLOCKED_PROJECTS"] != "agentic/gonk-project" {
		t.Fatalf("GONK_BLOCKED_PROJECTS = %q in the default render, want agentic/gonk-project", env["GONK_BLOCKED_PROJECTS"])
	}
}

// without returns args with every `--set key=...` for key removed.
func without(t *testing.T, args []string, key string) []string {
	t.Helper()
	var out []string
	var dropped int
	for i := 0; i < len(args); i++ {
		if (args[i] == "--set" || args[i] == "--set-json") && i+1 < len(args) && strings.HasPrefix(args[i+1], key+"=") {
			i++
			dropped++
			continue
		}
		out = append(out, args[i])
	}
	if dropped != 1 {
		t.Fatalf("Minimum() sets %s %d times, want exactly once: this test removes it to show it is required", key, dropped)
	}
	return out
}

// gitlab.url HAS NO DEFAULT. Minimum() supplies it; without it the render is
// refused by the schema, by name. (Before: a default pointed every render at
// one site's GitLab, and leaving the value out rendered.)
func TestGitLabURLIsRequired(t *testing.T) {
	out, err := RenderErr(t, without(t, Minimum(), "gitlab.url")...)
	if err == nil {
		t.Fatal("the chart rendered with no gitlab.url: it has a default GitLab again")
	}
	if !strings.Contains(out, "/gitlab/url") {
		t.Fatalf("the render failed, but not on gitlab.url:\n%s", out)
	}
}

// gonkWorkloads are the three that talk to GitLab, by their main container.
var gonkWorkloads = []struct{ deployment, container string }{
	{"gonk-intake", "intake"},
	{"gonk-meter", "meter"},
	{"gonk-controller", "controller"},
}

const caVolume = "gitlab-ca"

// THE ONE TRUST VARIABLE the chart sets, and its value for a bundle mounted at
// mountPath: the system directory FIRST, then the mounted file's directory.
// The private CA is added to the public roots, never put in their place;
// internal/catrust pins that this value does that in a Go process.
const caTrustVar = "SSL_CERT_DIR"

func caTrustValue(mountPath string) string { return "/etc/ssl/certs:" + path.Dir(mountPath) }

// replacingTrustVars are the variables that would put the private bundle IN
// PLACE of the system roots (or that only some other client reads). The chart
// sets none of them, with a CA or without.
var replacingTrustVars = []string{"SSL_CERT_FILE", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}

// rawEnv is a container's env by name (value "" for a valueFrom entry).
func rawEnv(c map[string]any) map[string]string {
	env := map[string]string{}
	envs, _ := c["env"].([]any)
	for _, e := range envs {
		em, _ := e.(map[string]any)
		n, _ := em["name"].(string)
		v, _ := em["value"].(string)
		env[n] = v
	}
	return env
}

// caMount returns the container's mount of the CA volume, or nil.
func caMount(c map[string]any) map[string]any {
	mounts, _ := c["volumeMounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == caVolume {
			return mm
		}
	}
	return nil
}

// WITH NO CA CONFIGURED, NO CA IS MOUNTED: no volume, no mount, and the chart
// sets NO trust variable in any container (the images set none either --
// internal/buildgate/image_trust_test.go -- so the base image's public roots
// are what is used).
func TestDefaultRenderMountsNoPrivateCA(t *testing.T) {
	out := Render(t, Minimum()...)
	for _, w := range gonkWorkloads {
		d := MustObject(t, out, "Deployment", w.deployment)
		if _, ok := podVolumes(t, d)[caVolume]; ok {
			t.Errorf("%s has a %s volume in the default render; no CA ConfigMap is configured", w.deployment, caVolume)
		}
		for name, c := range controllerContainers(t, d) {
			if name == "__pod" {
				continue
			}
			if m := caMount(c); m != nil {
				t.Errorf("%s/%s mounts %s in the default render: %v", w.deployment, name, caVolume, m)
			}
			env := rawEnv(c)
			for _, v := range append([]string{caTrustVar}, replacingTrustVars...) {
				if got, ok := env[v]; ok {
					t.Errorf("%s/%s sets %s=%q with no CA configured", w.deployment, name, v, got)
				}
			}
		}
		for _, vol := range podVolumes(t, d) {
			if cm, ok := vol.(map[string]any)["configMap"].(map[string]any); ok {
				for _, siteName := range []string{"trust-" + "bundle"} {
					if cm["name"] == siteName {
						t.Errorf("%s mounts ConfigMap %q in the default render: a site's trust bundle is a default again", w.deployment, siteName)
					}
				}
			}
		}
	}
}

// A CONFIGURED CA REACHES EVERY WORKLOAD, at the default path: the volume is
// the named ConfigMap's one key, the main container mounts it as a FILE
// (subPath) at mountPath, and SSL_CERT_DIR lists the system directory and then
// that file's directory. EVERY container of the pod is walked, init containers
// too: one that mounts the bundle has the variable, one that has the variable
// mounts the bundle, and none has a variable that would replace the roots.
func TestPrivateCAIsMountedInEveryWorkload(t *testing.T) {
	const cm, key = "corp-ca-bundle", "bundle.pem"
	out := Render(t, append(Minimum(),
		"--set", "gitlab.caCert.existingConfigMap="+cm,
		"--set", "gitlab.caCert.key="+key)...)
	for _, w := range gonkWorkloads {
		d := MustObject(t, out, "Deployment", w.deployment)
		vol, ok := podVolumes(t, d)[caVolume].(map[string]any)
		if !ok {
			t.Errorf("%s has no %s volume", w.deployment, caVolume)
			continue
		}
		src, _ := vol["configMap"].(map[string]any)
		items := toSliceOfMaps(t, src["items"])
		if src["name"] != cm || len(items) != 1 || items[0]["key"] != key {
			t.Errorf("%s volume %s = %v, want ConfigMap %s with the one key %s", w.deployment, caVolume, vol, cm, key)
		}
		c := containerByName(t, d, w.container)
		m := caMount(c)
		if m == nil || m["mountPath"] != defaultCAMountPath || m["subPath"] != key || m["readOnly"] != true {
			t.Errorf("%s/%s mount of %s = %v, want %s (subPath %s, readOnly)", w.deployment, w.container, caVolume, m, defaultCAMountPath, key)
		}
		want := caTrustValue(defaultCAMountPath)
		if want != "/etc/ssl/certs:/etc/ssl/gonk" {
			t.Fatalf("caTrustValue(%s) = %q: the test's own expectation moved", defaultCAMountPath, want)
		}
		if got := rawEnv(c)[caTrustVar]; got != want {
			t.Errorf("%s/%s %s = %q, want %q (system roots, then the mounted directory)", w.deployment, w.container, caTrustVar, got, want)
		}
		mounting := 0
		for name, cc := range controllerContainers(t, d) {
			if name == "__pod" {
				continue
			}
			env := rawEnv(cc)
			got, has := env[caTrustVar]
			mounted := caMount(cc) != nil
			if mounted {
				mounting++
			}
			if mounted != has {
				t.Errorf("%s/%s: mounts the CA = %v but sets %s = %v; a container has both or neither", w.deployment, name, mounted, caTrustVar, has)
			}
			if has && got != want {
				t.Errorf("%s/%s %s = %q, want %q", w.deployment, name, caTrustVar, got, want)
			}
			for _, v := range replacingTrustVars {
				if val, ok := env[v]; ok {
					t.Errorf("%s/%s sets %s=%q: that REPLACES the system roots (or is read by a client the chart does not run); the private CA is added with %s", w.deployment, name, v, val, caTrustVar)
				}
			}
		}
		if mounting == 0 {
			t.Errorf("%s: no container mounts the CA", w.deployment)
		}
	}
	// The controller pod has two containers that reach GitLab or the stores.
	both := controllerContainers(t, MustObject(t, out, "Deployment", "gonk-controller"))
	for _, name := range []string{"bootstrap-city", "controller"} {
		if c, ok := both[name]; !ok || caMount(c) == nil {
			t.Errorf("gonk-controller/%s does not mount the CA", name)
		}
	}
}

// mountPath and SSL_CERT_DIR move TOGETHER: the variable's second directory is
// always the directory of the mounted file, because both come from one value.
func TestPrivateCAMountPathAndEnvMoveTogether(t *testing.T) {
	const mountPath = "/etc/pki/site/ca.pem"
	out := Render(t, append(Minimum(),
		"--set", "gitlab.caCert.existingConfigMap=corp-ca-bundle",
		"--set", "gitlab.caCert.key=bundle.pem",
		"--set", "gitlab.caCert.mountPath="+mountPath)...)
	for _, w := range gonkWorkloads {
		c := containerByName(t, MustObject(t, out, "Deployment", w.deployment), w.container)
		m := caMount(c)
		if m == nil || m["mountPath"] != mountPath {
			t.Errorf("%s/%s mounts the CA at %v, want %s", w.deployment, w.container, m, mountPath)
		}
		if got, want := rawEnv(c)[caTrustVar], "/etc/ssl/certs:/etc/pki/site"; got != want {
			t.Errorf("%s/%s %s = %q, want %q", w.deployment, w.container, caTrustVar, got, want)
		}
	}
}

// G25: half a CA configuration is refused, by name.
func TestGuardCAConfigMapWithoutKey(t *testing.T) {
	mustFail(t, "gitlab.caCert.key is empty",
		"--set", "gitlab.caCert.existingConfigMap=corp-ca-bundle")
}

func TestGuardCAKeyWithoutConfigMap(t *testing.T) {
	mustFail(t, "gitlab.caCert.existingConfigMap is empty",
		"--set", "gitlab.caCert.key=bundle.pem")
}

func TestGuardCAWithoutMountPath(t *testing.T) {
	mustFail(t, "gitlab.caCert.mountPath is empty",
		"--set", "gitlab.caCert.existingConfigMap=corp-ca-bundle",
		"--set", "gitlab.caCert.key=bundle.pem",
		"--set", "gitlab.caCert.mountPath=")
}

// podSpecs returns the pod spec of every pod-bearing object in a render.
func podSpecs(objs []Object) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, o := range objs {
		var pspec map[string]any
		switch o.Kind {
		case "Pod":
			pspec, _ = o.Raw["spec"].(map[string]any)
		case "Deployment", "StatefulSet", "Job", "DaemonSet":
			spec, _ := o.Raw["spec"].(map[string]any)
			tmpl, _ := spec["template"].(map[string]any)
			pspec, _ = tmpl["spec"].(map[string]any)
		}
		if pspec != nil {
			out[o.Kind+"/"+o.Metadata.Name] = pspec
		}
	}
	return out
}

// THE DEFAULT RENDER PULLS FROM THE PUBLIC REGISTRY WITH NO PULL SECRET: every
// gonk image is under publicRegistry, and nothing -- no pod, and not the
// ServiceAccount session pods inherit theirs from -- names a pull Secret.
func TestDefaultRenderUsesThePublicRegistryAndNoPullSecret(t *testing.T) {
	objs := Objects(t, Render(t, Minimum()...))
	uses := gonkImageUses(t, objs)
	seen := map[string]bool{}
	for _, u := range uses {
		seen[u.image] = true
		if u.registry != publicRegistry {
			t.Errorf("%s pulls %s from %q, want the default registry %q", u.where, u.image, u.registry, publicRegistry)
		}
	}
	for _, img := range []string{"gonk-controller", "gonk-intake", "gonk-meter", "gonk-agent"} {
		if !seen[img] {
			t.Errorf("no %s image in the default render, so nothing was asserted about its registry", img)
		}
	}
	pods := podSpecs(objs)
	if len(pods) < 4 {
		t.Fatalf("only %d pod-bearing objects in the default render", len(pods))
	}
	for name, p := range pods {
		if ps, ok := p["imagePullSecrets"]; ok {
			t.Errorf("%s has imagePullSecrets %v in the default render", name, ps)
		}
	}
	for _, o := range objs {
		if o.Kind == "ServiceAccount" {
			if ps, ok := o.Raw["imagePullSecrets"]; ok {
				t.Errorf("ServiceAccount %s has imagePullSecrets %v in the default render", o.Metadata.Name, ps)
			}
		}
	}
}

// A PRIVATE MIRROR IS TWO VALUES: every gonk image moves to image.registry and
// every pod (and the agents' ServiceAccount) carries the pull Secret. This is
// the reference cluster's configuration, so it is the override that must work.
func TestPrivateRegistryAndPullSecretReachEveryPod(t *testing.T) {
	const reg, secret = "registry.example.internal/group/project", "registry-pull-creds"
	objs := Objects(t, Render(t, append(Minimum(),
		"--set", "image.registry="+reg,
		"--set", "image.pullSecrets[0].name="+secret)...))
	uses := gonkImageUses(t, objs)
	if len(uses) < 4 {
		t.Fatalf("only %d gonk image uses in the render", len(uses))
	}
	for _, u := range uses {
		if u.registry != reg {
			t.Errorf("%s pulls %s from %q, want %q", u.where, u.image, u.registry, reg)
		}
	}
	names := func(v any) []string {
		var out []string
		for _, p := range toSliceOfMaps(t, v) {
			s, _ := p["name"].(string)
			out = append(out, s)
		}
		return out
	}
	for name, p := range podSpecs(objs) {
		if got := names(p["imagePullSecrets"]); !containsStr(got, secret) {
			t.Errorf("%s imagePullSecrets = %v, want %s", name, got, secret)
		}
	}
	if got := names(MustObject(t, Render(t, append(Minimum(),
		"--set", "image.registry="+reg,
		"--set", "image.pullSecrets[0].name="+secret)...), "ServiceAccount", "gc-agent").Raw["imagePullSecrets"]); !containsStr(got, secret) {
		t.Errorf("ServiceAccount gc-agent imagePullSecrets = %v, want %s: a session pod running as it could not pull the agent image", got, secret)
	}
}

// THE INGRESS HAS NO DEFAULT HOST AND NO DEFAULT ANNOTATIONS. Enabled with a
// host, it renders exactly that host and no annotation naming an issuer or a
// DNS record -- those are the operator's to add.
func TestIngressCarriesOnlyWhatTheOperatorSets(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "ingress.enabled=true",
		"--set", "ingress.host=gonk.example.test")...)
	ing := MustObject(t, out, "Ingress", "gonk-intake")
	if len(ing.Metadata.Annotations) != 0 {
		t.Errorf("Ingress annotations = %v, want none by default", ing.Metadata.Annotations)
	}
	spec, _ := ing.Raw["spec"].(map[string]any)
	rules := toSliceOfMaps(t, spec["rules"])
	if len(rules) != 1 || rules[0]["host"] != "gonk.example.test" {
		t.Errorf("Ingress rules = %v, want the one host gonk.example.test", rules)
	}

	out = Render(t, append(Minimum(),
		"--set", "ingress.enabled=true",
		"--set", "ingress.host=gonk.example.test",
		"--set", `ingress.annotations.cert-manager\.io/cluster-issuer=site-issuer`)...)
	ing = MustObject(t, out, "Ingress", "gonk-intake")
	if got := ing.Metadata.Annotations["cert-manager.io/cluster-issuer"]; got != "site-issuer" {
		t.Errorf("Ingress cluster-issuer annotation = %q, want site-issuer", got)
	}
}
