/**
 * resolve.ts — repo → namespace.
 *
 * Given a repo's GHCR image, find the running deployments using it and return
 * their namespace(s). An app may span several namespaces (mailon, mailon-staging);
 * we also collapse them into `<app>-*` groups so the views can zoom out from a
 * namespace to its app group. The entry-point view uses this to land the user
 * on the right namespace when kc is started inside a repo.
 */

import { getAllDeployments } from "./k8s/index.ts"
import type { K8sOptions } from "./k8s/index.ts"
import type { Deployment, RepoResolution, ResolvedNamespace } from "./types.ts"

/** Does a deployment's image (any container) match the target GHCR repository? */
function deploymentMatchesImage(dep: Deployment, image: string): boolean {
  const want = image.toLowerCase()
  return dep.images.some((img) => img.repository.toLowerCase() === want)
}

/**
 * Pure core: given all deployments and a target image, group matching
 * deployments by namespace and roll namespaces up into `<app>-*` groups.
 *
 * Group key = the namespace name truncated at the first "-" (so `mailon` and
 * `mailon-staging` share group "mailon"). A namespace with no "-" is its own
 * group. Groups and their namespaces are returned sorted for stable output.
 */
export function resolveFromDeployments(
  deployments: Deployment[],
  image: string,
): RepoResolution {
  const byNamespace = new Map<string, string[]>()
  for (const dep of deployments) {
    if (!deploymentMatchesImage(dep, image)) continue
    const list = byNamespace.get(dep.namespace) ?? []
    list.push(dep.name)
    byNamespace.set(dep.namespace, list)
  }

  const namespaces: ResolvedNamespace[] = [...byNamespace.entries()]
    .map(([namespace, deps]) => ({ namespace, deployments: deps.sort() }))
    .sort((a, b) => a.namespace.localeCompare(b.namespace))

  // Collapse namespaces into <app>-* groups.
  const byApp = new Map<string, string[]>()
  for (const { namespace } of namespaces) {
    const app = namespace.includes("-") ? namespace.slice(0, namespace.indexOf("-")) : namespace
    const list = byApp.get(app) ?? []
    list.push(namespace)
    byApp.set(app, list)
  }
  const groups = [...byApp.entries()]
    .map(([app, ns]) => ({ app, namespaces: ns.sort() }))
    .sort((a, b) => a.app.localeCompare(b.app))

  return { image, namespaces, groups }
}

/**
 * Resolve a GHCR image to the namespaces running it, live.
 *
 * `image` is typically `RepoContext.ghcrImage`. A null/empty image (not in a
 * repo, or no origin) yields an empty resolution rather than erroring.
 */
export async function resolveNamespaces(
  image: string | null | undefined,
  opts: K8sOptions = {},
): Promise<RepoResolution> {
  if (!image) return { image: null, namespaces: [], groups: [] }
  const deployments = await getAllDeployments(opts)
  return resolveFromDeployments(deployments, image)
}
