/**
 * Unit tests for repo → namespace resolution (pure core), incl. multi-namespace
 * grouping by `<app>-*`.
 */

import { test, expect, describe } from "bun:test"
import { resolveFromDeployments } from "../src/resolve.ts"
import { parseImage } from "../src/k8s/parse.ts"
import type { Deployment } from "../src/types.ts"

function dep(namespace: string, name: string, image: string): Deployment {
  const img = parseImage(image)
  return {
    namespace,
    name,
    image: img,
    images: [img],
    readyReplicas: 1,
    desiredReplicas: 1,
    usage: null,
  }
}

describe("resolveFromDeployments", () => {
  test("matches deployments by image repository across namespaces", () => {
    const deployments = [
      dep("mailon", "web", "ghcr.io/thinkpilot/mailon:v0.6.9"),
      dep("mailon", "sender", "ghcr.io/thinkpilot/mailon:v0.6.9"),
      dep("mailon-staging", "web", "ghcr.io/thinkpilot/mailon:v0.6.8"),
      dep("consistant", "api", "ghcr.io/thinkpilot/consistant:v1.0.0"), // unrelated
    ]
    const res = resolveFromDeployments(deployments, "ghcr.io/thinkpilot/mailon")

    expect(res.image).toBe("ghcr.io/thinkpilot/mailon")
    expect(res.namespaces.map((n) => n.namespace)).toEqual(["mailon", "mailon-staging"])
    // deployments within a namespace are sorted.
    expect(res.namespaces.find((n) => n.namespace === "mailon")!.deployments).toEqual([
      "sender",
      "web",
    ])
  })

  test("collapses namespaces into <app>-* groups", () => {
    const deployments = [
      dep("mailon", "web", "ghcr.io/thinkpilot/mailon:v1"),
      dep("mailon-staging", "web", "ghcr.io/thinkpilot/mailon:v1"),
    ]
    const res = resolveFromDeployments(deployments, "ghcr.io/thinkpilot/mailon")
    expect(res.groups).toEqual([{ app: "mailon", namespaces: ["mailon", "mailon-staging"] }])
  })

  test("matching is tag-insensitive and case-insensitive on repository", () => {
    const deployments = [dep("mailon", "web", "ghcr.io/ThinkPilot/Mailon:v9")]
    const res = resolveFromDeployments(deployments, "ghcr.io/thinkpilot/mailon")
    expect(res.namespaces.length).toBe(1)
  })

  test("no match → empty resolution", () => {
    const deployments = [dep("other", "x", "docker.io/library/nginx:1")]
    const res = resolveFromDeployments(deployments, "ghcr.io/thinkpilot/mailon")
    expect(res.namespaces).toEqual([])
    expect(res.groups).toEqual([])
  })
})
