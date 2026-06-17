/**
 * Unit tests for git-remote parsing (ssh + https) and GHCR derivation.
 * Pure string logic — no git invocation.
 */

import { test, expect, describe } from "bun:test"
import { parseRemote, ghcrImageFor } from "../src/git.ts"

describe("parseRemote", () => {
  test("ssh scp-like (the team's mailon remote)", () => {
    expect(parseRemote("git@github.com:thinkpilot/mailon.git")).toEqual({
      owner: "thinkpilot",
      repo: "mailon",
    })
  })
  test("ssh scp-like without .git", () => {
    expect(parseRemote("git@github.com:thinkpilot/mailon")).toEqual({
      owner: "thinkpilot",
      repo: "mailon",
    })
  })
  test("ssh url form", () => {
    expect(parseRemote("ssh://git@github.com/thinkpilot/mailon.git")).toEqual({
      owner: "thinkpilot",
      repo: "mailon",
    })
  })
  test("https with .git", () => {
    expect(parseRemote("https://github.com/thinkpilot/mailon.git")).toEqual({
      owner: "thinkpilot",
      repo: "mailon",
    })
  })
  test("https without .git, trailing slash", () => {
    expect(parseRemote("https://github.com/thinkpilot/mailon/")).toEqual({
      owner: "thinkpilot",
      repo: "mailon",
    })
  })
  test("https with embedded user/token", () => {
    expect(parseRemote("https://user@github.com/thinkpilot/mailon.git")).toEqual({
      owner: "thinkpilot",
      repo: "mailon",
    })
  })
  test("git:// protocol", () => {
    expect(parseRemote("git://github.com/owner/repo.git")).toEqual({
      owner: "owner",
      repo: "repo",
    })
  })
  test("whitespace tolerated", () => {
    expect(parseRemote("  git@github.com:thinkpilot/mailon.git\n")).toEqual({
      owner: "thinkpilot",
      repo: "mailon",
    })
  })
  test("garbage / empty → null", () => {
    expect(parseRemote("")).toBeNull()
    expect(parseRemote("not-a-remote")).toBeNull()
    expect(parseRemote("git@github.com:onlyowner")).toBeNull()
  })
})

describe("ghcrImageFor", () => {
  test("derives ghcr.io/<owner>/<repo>", () => {
    expect(ghcrImageFor({ owner: "thinkpilot", repo: "mailon" })).toBe(
      "ghcr.io/thinkpilot/mailon",
    )
  })
  test("lowercases owner/repo (GHCR requires lowercase)", () => {
    expect(ghcrImageFor({ owner: "ThinkPilot", repo: "MailOn" })).toBe(
      "ghcr.io/thinkpilot/mailon",
    )
  })
})
