package resolve

import (
	"reflect"
	"testing"

	"github.com/backhand/kc/internal/k8s"
)

// Unit tests for repo → namespace resolution (pure core), incl. multi-namespace
// grouping by `<app>-*`.
// Ported from tools/kc-bun/test/resolve.test.ts.

func dep(namespace, name, image string) k8s.Deployment {
	img := k8s.ParseImage(image)
	return k8s.Deployment{
		Namespace:       namespace,
		Name:            name,
		Image:           img,
		Images:          []k8s.ImageRef{img},
		ReadyReplicas:   1,
		DesiredReplicas: 1,
	}
}

func nsNames(rs []ResolvedNamespace) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Namespace
	}
	return out
}

func TestFromDeployments_MatchesByRepositoryAcrossNamespaces(t *testing.T) {
	deployments := []k8s.Deployment{
		dep("mailon", "web", "ghcr.io/thinkpilot/mailon:v0.6.9"),
		dep("mailon", "sender", "ghcr.io/thinkpilot/mailon:v0.6.9"),
		dep("mailon-staging", "web", "ghcr.io/thinkpilot/mailon:v0.6.8"),
		dep("consistant", "api", "ghcr.io/thinkpilot/consistant:v1.0.0"), // unrelated
	}
	res := FromDeployments(deployments, "ghcr.io/thinkpilot/mailon")

	if res.Image != "ghcr.io/thinkpilot/mailon" {
		t.Errorf("image = %q", res.Image)
	}
	if got := nsNames(res.Namespaces); !reflect.DeepEqual(got, []string{"mailon", "mailon-staging"}) {
		t.Errorf("namespaces = %v, want [mailon mailon-staging]", got)
	}
	for _, rn := range res.Namespaces {
		if rn.Namespace == "mailon" {
			if !reflect.DeepEqual(rn.Deployments, []string{"sender", "web"}) {
				t.Errorf("mailon deployments = %v, want [sender web] (sorted)", rn.Deployments)
			}
		}
	}
}

func TestFromDeployments_CollapsesIntoAppGroups(t *testing.T) {
	deployments := []k8s.Deployment{
		dep("mailon", "web", "ghcr.io/thinkpilot/mailon:v1"),
		dep("mailon-staging", "web", "ghcr.io/thinkpilot/mailon:v1"),
	}
	res := FromDeployments(deployments, "ghcr.io/thinkpilot/mailon")
	want := []Group{{App: "mailon", Namespaces: []string{"mailon", "mailon-staging"}}}
	if !reflect.DeepEqual(res.Groups, want) {
		t.Errorf("groups = %+v, want %+v", res.Groups, want)
	}
}

func TestFromDeployments_TagAndCaseInsensitive(t *testing.T) {
	deployments := []k8s.Deployment{dep("mailon", "web", "ghcr.io/ThinkPilot/Mailon:v9")}
	res := FromDeployments(deployments, "ghcr.io/thinkpilot/mailon")
	if len(res.Namespaces) != 1 {
		t.Errorf("got %d namespaces, want 1 (case-insensitive match)", len(res.Namespaces))
	}
}

func TestFromDeployments_NoMatch(t *testing.T) {
	deployments := []k8s.Deployment{dep("other", "x", "docker.io/library/nginx:1")}
	res := FromDeployments(deployments, "ghcr.io/thinkpilot/mailon")
	if len(res.Namespaces) != 0 {
		t.Errorf("namespaces = %v, want empty", res.Namespaces)
	}
	if len(res.Groups) != 0 {
		t.Errorf("groups = %v, want empty", res.Groups)
	}
}
