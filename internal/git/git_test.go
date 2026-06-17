package git

import "testing"

// Unit tests for git-remote parsing (ssh + https) and GHCR derivation.
// Pure string logic — no git invocation.
// Ported from tools/kc-bun/test/git.test.ts.

func TestParseRemote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *RepoRef
	}{
		{"ssh scp-like (the team's mailon remote)", "git@github.com:thinkpilot/mailon.git", &RepoRef{"thinkpilot", "mailon"}},
		{"ssh scp-like without .git", "git@github.com:thinkpilot/mailon", &RepoRef{"thinkpilot", "mailon"}},
		{"ssh url form", "ssh://git@github.com/thinkpilot/mailon.git", &RepoRef{"thinkpilot", "mailon"}},
		{"https with .git", "https://github.com/thinkpilot/mailon.git", &RepoRef{"thinkpilot", "mailon"}},
		{"https without .git, trailing slash", "https://github.com/thinkpilot/mailon/", &RepoRef{"thinkpilot", "mailon"}},
		{"https with embedded user/token", "https://user@github.com/thinkpilot/mailon.git", &RepoRef{"thinkpilot", "mailon"}},
		{"git:// protocol", "git://github.com/owner/repo.git", &RepoRef{"owner", "repo"}},
		{"whitespace tolerated", "  git@github.com:thinkpilot/mailon.git\n", &RepoRef{"thinkpilot", "mailon"}},
		{"empty → nil", "", nil},
		{"garbage → nil", "not-a-remote", nil},
		{"only owner → nil", "git@github.com:onlyowner", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseRemote(c.in)
			if c.want == nil {
				if got != nil {
					t.Errorf("ParseRemote(%q) = %+v, want nil", c.in, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseRemote(%q) = nil, want %+v", c.in, *c.want)
			}
			if *got != *c.want {
				t.Errorf("ParseRemote(%q) = %+v, want %+v", c.in, *got, *c.want)
			}
		})
	}
}

func TestGHCRImageFor(t *testing.T) {
	if got := GHCRImageFor(RepoRef{Owner: "thinkpilot", Repo: "mailon"}); got != "ghcr.io/thinkpilot/mailon" {
		t.Errorf("GHCRImageFor = %q, want ghcr.io/thinkpilot/mailon", got)
	}
	// GHCR requires lowercase.
	if got := GHCRImageFor(RepoRef{Owner: "ThinkPilot", Repo: "MailOn"}); got != "ghcr.io/thinkpilot/mailon" {
		t.Errorf("GHCRImageFor (mixed case) = %q, want ghcr.io/thinkpilot/mailon", got)
	}
}
