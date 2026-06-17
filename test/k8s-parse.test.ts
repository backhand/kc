/**
 * Unit tests for the pure kubectl-JSON parsers, against captured live fixtures.
 * Deterministic and offline — no cluster access.
 */

import { test, expect, describe } from "bun:test"
import { join } from "node:path"
import {
  parseCpuToMillicores,
  parseMemoryToBytes,
  parseImage,
  parseNodes,
  parseNamespaces,
  parseDeployments,
  parsePods,
  buildPodToDeployment,
  classifyNamespace,
  SYSTEM_NAMESPACES,
} from "../src/k8s/parse.ts"
import type {
  RawList,
  RawNode,
  RawNamespace,
  RawDeployment,
  RawReplicaSet,
  RawPod,
  RawNodeMetricsList,
  RawPodMetricsList,
} from "../src/k8s/raw.ts"

const FIX = join(import.meta.dir, "fixtures")
async function fixture<T>(name: string): Promise<T> {
  return (await Bun.file(join(FIX, name)).json()) as T
}

// ── Quantity parsing ────────────────────────────────────────────────────────

describe("parseCpuToMillicores", () => {
  test("millicores, cores, nano, micro", () => {
    expect(parseCpuToMillicores("250m")).toBe(250)
    expect(parseCpuToMillicores("2")).toBe(2000)
    expect(parseCpuToMillicores("0.5")).toBe(500)
    expect(parseCpuToMillicores("1500000000n")).toBe(1500)
    expect(parseCpuToMillicores("279789138n")).toBe(280) // from node-metrics fixture
    expect(parseCpuToMillicores("500u")).toBe(1) // 500 micro = 0.5m → Math.round → 1
    expect(parseCpuToMillicores("5000u")).toBe(5) // 5000 micro = 5m
  })
  test("empty / null → 0", () => {
    expect(parseCpuToMillicores(undefined)).toBe(0)
    expect(parseCpuToMillicores("")).toBe(0)
    expect(parseCpuToMillicores(null)).toBe(0)
  })
})

describe("parseMemoryToBytes", () => {
  test("binary suffixes", () => {
    expect(parseMemoryToBytes("128Mi")).toBe(134217728)
    expect(parseMemoryToBytes("2Gi")).toBe(2147483648)
    expect(parseMemoryToBytes("7931600Ki")).toBe(7931600 * 1024)
  })
  test("decimal suffixes & bare bytes", () => {
    expect(parseMemoryToBytes("500M")).toBe(500000000)
    expect(parseMemoryToBytes("1024")).toBe(1024)
  })
  test("milli suffix (fractional bytes from some kubelets)", () => {
    // 512000000m = 512000000 / 1000 = 512000 bytes
    expect(parseMemoryToBytes("512000000m")).toBe(512000)
    expect(parseMemoryToBytes("1500m")).toBe(2) // 1.5 bytes → round
  })
  test("garbage value degrades to 0, never NaN (must not poison sums)", () => {
    expect(parseMemoryToBytes("not-a-number")).toBe(0)
    expect(parseMemoryToBytes("12Xi")).toBe(0)
    expect(Number.isNaN(parseMemoryToBytes("Mi"))).toBe(false)
  })
  test("empty / null → 0", () => {
    expect(parseMemoryToBytes(undefined)).toBe(0)
    expect(parseMemoryToBytes("")).toBe(0)
  })
})

// ── Image parsing ────────────────────────────────────────────────────────

describe("parseImage", () => {
  test("registry/owner/repo:tag", () => {
    const i = parseImage("ghcr.io/thinkpilot/mailon:v0.6.9")
    expect(i.repository).toBe("ghcr.io/thinkpilot/mailon")
    expect(i.tag).toBe("v0.6.9")
    expect(i.digest).toBeNull()
  })
  test("no tag → null tag", () => {
    const i = parseImage("ghcr.io/thinkpilot/mailon")
    expect(i.repository).toBe("ghcr.io/thinkpilot/mailon")
    expect(i.tag).toBeNull()
  })
  test("registry with port is not mistaken for a tag", () => {
    const i = parseImage("localhost:5000/app:1.0")
    expect(i.repository).toBe("localhost:5000/app")
    expect(i.tag).toBe("1.0")
  })
  test("port, no tag", () => {
    const i = parseImage("localhost:5000/app")
    expect(i.repository).toBe("localhost:5000/app")
    expect(i.tag).toBeNull()
  })
  test("digest pin", () => {
    const i = parseImage("ghcr.io/thinkpilot/mailon@sha256:abc123")
    expect(i.repository).toBe("ghcr.io/thinkpilot/mailon")
    expect(i.tag).toBeNull()
    expect(i.digest).toBe("sha256:abc123")
  })
  test("tag AND digest — tag must NOT glue onto repository", () => {
    // A GitOps digest-pinner can write repo:tag@sha256. Resolution does an
    // exact repository match, so the tag must be split out cleanly.
    const i = parseImage("ghcr.io/thinkpilot/mailon:v0.6.9@sha256:abc123")
    expect(i.repository).toBe("ghcr.io/thinkpilot/mailon")
    expect(i.tag).toBe("v0.6.9")
    expect(i.digest).toBe("sha256:abc123")
  })
})

// ── Namespace classification ────────────────────────────────────────────────

describe("classifyNamespace", () => {
  test("kube-* prefix → system", () => {
    expect(classifyNamespace("kube-system")).toBe("system")
    expect(classifyNamespace("kube-public")).toBe("system")
    expect(classifyNamespace("kube-node-lease")).toBe("system")
  })
  test("denylist → system", () => {
    for (const ns of ["cert-manager", "dex", "buildkit", "actions-runner"]) {
      expect(classifyNamespace(ns)).toBe("system")
    }
  })
  test("app namespaces → user", () => {
    for (const ns of ["mailon", "consistant", "temporal", "default"]) {
      expect(classifyNamespace(ns)).toBe("user")
    }
  })
  test("denylist constant contains the expected systems", () => {
    expect(SYSTEM_NAMESPACES).toContain("cert-manager")
    expect(SYSTEM_NAMESPACES).toContain("dex")
    expect(SYSTEM_NAMESPACES).toContain("buildkit")
    expect(SYSTEM_NAMESPACES).toContain("actions-runner")
  })
})

describe("parseNamespaces (fixture)", () => {
  test("user namespaces sorted before system", async () => {
    const list = await fixture<RawList<RawNamespace>>("namespaces.json")
    const parsed = parseNamespaces(list.items)
    const kinds = parsed.map((n) => n.kind)
    // No "user" appears after a "system".
    const firstSystem = kinds.indexOf("system")
    const lastUser = kinds.lastIndexOf("user")
    expect(lastUser).toBeLessThan(firstSystem)
    // Live cluster sanity.
    const names = parsed.map((n) => n.name)
    expect(names).toContain("mailon")
    expect(names).toContain("kube-system")
    expect(parsed.find((n) => n.name === "mailon")!.kind).toBe("user")
    expect(parsed.find((n) => n.name === "kube-system")!.kind).toBe("system")
  })
})

// ── Nodes (fixture) ────────────────────────────────────────────────────────

describe("parseNodes (fixture)", () => {
  test("roles, control-plane flag, capacity, usage merge, sort", async () => {
    const list = await fixture<RawList<RawNode>>("nodes.json")
    const metrics = await fixture<RawNodeMetricsList>("node-metrics.json")
    const nodes = parseNodes(list.items, metrics.items)

    // Two nodes; control-plane first.
    expect(nodes.length).toBe(2)
    expect(nodes[0]!.controlPlane).toBe(true)
    expect(nodes[0]!.roles).toContain("control-plane")

    const server = nodes.find((n) => n.name === "thinkpilot-k3s-server")!
    expect(server.ready).toBe(true)
    expect(server.kubeletVersion).toMatch(/^v1\./)
    // 4 cores allocatable.
    const agent = nodes.find((n) => n.name === "thinkpilot-k3s-agent-0")!
    expect(agent.capacity.cpuMillicores).toBe(4000)
    // Usage merged from metrics (agent ~280m from fixture).
    expect(agent.usage).not.toBeNull()
    expect(agent.usage!.cpuMillicores).toBeGreaterThan(0)
  })

  test("no metrics → usage null", async () => {
    const list = await fixture<RawList<RawNode>>("nodes.json")
    const nodes = parseNodes(list.items, [])
    expect(nodes.every((n) => n.usage === null)).toBe(true)
  })
})

// ── Pod → deployment mapping (fixture) ──────────────────────────────────────

describe("buildPodToDeployment (fixture)", () => {
  test("maps running pods through RS ownerRef chain to deployments", async () => {
    const pods = await fixture<RawList<RawPod>>("mailon-pods.json")
    const rs = await fixture<RawList<RawReplicaSet>>("mailon-replicasets.json")
    const map = buildPodToDeployment(pods.items, rs.items)

    // Every mailon pod resolves to one of the five deployments.
    const deployments = new Set(["ingester", "knowledge", "responder", "sender", "web"])
    for (const [, dep] of map) {
      if (dep !== null) expect(deployments.has(dep)).toBe(true)
    }
    // Spot-check a known running pod from the fixture.
    const ingesterPod = pods.items.find((p) => p.metadata.name.startsWith("ingester-"))!
    expect(map.get(ingesterPod.metadata.name)).toBe("ingester")
  })
})

// ── Pods (fixture) ────────────────────────────────────────────────────────

describe("parsePods (fixture)", () => {
  test("status, node, restarts, deployment, usage", async () => {
    const pods = await fixture<RawList<RawPod>>("mailon-pods.json")
    const rs = await fixture<RawList<RawReplicaSet>>("mailon-replicasets.json")
    const metrics = await fixture<RawPodMetricsList>("mailon-pod-metrics.json")
    const parsed = parsePods(pods.items, rs.items, metrics.items)

    expect(parsed.length).toBeGreaterThan(0)
    const running = parsed.filter((p) => p.phase === "Running")
    // Running pods carry a node and a resolved deployment.
    for (const p of running) {
      expect(p.node).not.toBe("")
      expect(p.deployment).not.toBeNull()
    }
    // The 6 metric'd pods have usage; pods without metrics are null.
    const withUsage = parsed.filter((p) => p.usage !== null)
    expect(withUsage.length).toBe(metrics.items.length)
    expect(withUsage.every((p) => p.usage!.memoryBytes > 0)).toBe(true)
  })
})

// ── Deployments + per-deployment usage rollup (fixture) ─────────────────────

describe("parseDeployments (fixture)", () => {
  test("image/tag, ready/desired, per-deployment usage summed from pods", async () => {
    const deps = await fixture<RawList<RawDeployment>>("mailon-deployments.json")
    const pods = await fixture<RawList<RawPod>>("mailon-pods.json")
    const rs = await fixture<RawList<RawReplicaSet>>("mailon-replicasets.json")
    const metrics = await fixture<RawPodMetricsList>("mailon-pod-metrics.json")
    const parsed = parseDeployments(deps.items, pods.items, rs.items, metrics.items)

    const byName = new Map(parsed.map((d) => [d.name, d]))
    expect([...byName.keys()].sort()).toEqual([
      "ingester",
      "knowledge",
      "responder",
      "sender",
      "web",
    ])

    const web = byName.get("web")!
    expect(web.image.repository).toBe("ghcr.io/thinkpilot/mailon")
    expect(web.image.tag).toBe("v0.6.9")
    expect(web.desiredReplicas).toBe(2)
    expect(web.readyReplicas).toBe(2)

    // web has 2 pods in the metrics fixture; its usage = their sum.
    const webMetricPods = metrics.items.filter((m) => m.metadata.name.startsWith("web-"))
    const expectedMem = webMetricPods.reduce((sum, m) => {
      for (const c of m.containers ?? []) {
        const mem = c.usage?.memory ?? "0"
        sum += parseMemoryToBytes(mem)
      }
      return sum
    }, 0)
    expect(web.usage).not.toBeNull()
    expect(web.usage!.memoryBytes).toBe(expectedMem)
  })

  test("no metrics → per-deployment usage null", async () => {
    const deps = await fixture<RawList<RawDeployment>>("mailon-deployments.json")
    const pods = await fixture<RawList<RawPod>>("mailon-pods.json")
    const rs = await fixture<RawList<RawReplicaSet>>("mailon-replicasets.json")
    const parsed = parseDeployments(deps.items, pods.items, rs.items, [])
    expect(parsed.every((d) => d.usage === null)).toBe(true)
  })
})
