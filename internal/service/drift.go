package service

import (
	"context"

	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
)

// RunningImages is the matrix's cluster question: the raw pod observations for one env (the env
// is the namespace, AGENTS.md §1), straight from pkg/k8s, keyed by canonical image repo with
// every distinct running reference kept — never the planning resolver, which picks one digest
// per repo and would collapse a partial rollout, and which grafts the manifest's tag onto the
// digest it picked (#122). Pods only, whatever digest_sources the config orders for planning: a
// manifest or registry answer is not "what the cluster runs". Each running reference carries the
// pod's digest and, when the pod reported the image it was started from, that image's tag — so a
// bare manifest tag is compared with what the pod pulled, not with a tag the resolver assumed.
//
// The cluster is opened per call, so an unreachable one is one env's "cluster not asked"
// sentence, never a reason the TUI fails to open.
func (s *Service) RunningImages(ctx context.Context, env string) (map[string][]image.Ref, error) {
	cluster, _, err := s.Cluster(s.settings.KubeContext)
	if err != nil {
		return nil, err
	}
	imgs, err := cluster.RunningImages(ctx, env)
	if err != nil {
		return nil, err
	}
	return runningRefs(imgs), nil
}

// runningRefs groups pod observations by canonical image repo, one entry per distinct
// (digest, tag) the namespace runs. The tag comes from the pod's reported image when its
// repo is the same one as the imageID's; a pod that reported none, or a different repo (a
// mirror pulled under another name), contributes its digest alone.
func runningRefs(imgs []k8s.RunningImage) map[string][]image.Ref {
	out := map[string][]image.Ref{}
	seen := map[string]bool{}
	for _, ri := range imgs {
		ref := ri.Ref
		ref.Tag = ""
		if ri.Image.Tag != "" && image.Canonical(ri.Image.Repo) == image.Canonical(ri.Ref.Repo) {
			ref.Tag = ri.Image.Tag
		}
		key := image.Canonical(ref.Repo)
		if id := key + "\n" + ref.String(); !seen[id] {
			seen[id] = true
			out[key] = append(out[key], ref)
		}
	}
	return out
}
