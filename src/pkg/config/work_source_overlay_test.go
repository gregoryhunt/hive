package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadWithDashboardOverlay_AdoptsGovernorWorkSource reproduces the
// standalone-Kubernetes report: a work source set from the dashboard was
// written to /data/hive.yaml.dashboard but after a pod restart the reload
// only adopted OTel, RemovedAgents and Agents from the overlay, so
// GET /api/config/governor/work-source came back with type "".
func TestLoadWithDashboardOverlay_AdoptsGovernorWorkSource(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// The overlay is what saveConfig wrote after the dashboard PUT. It is
	// deliberately "short" (no agents) so the fullness guard would bail early:
	// the work source must be adopted regardless.
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
project:
  org: testorg
  repos: [repo1]
governor:
  work_source:
    type: linear
    linear:
      api_key: ${LINEAR_API_KEY}
      hold_labels: [blocked]
      teams:
        - key: ENG
          repo: testorg/repo1
`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	// Unset so the reference survives the overlay's env expansion; the
	// worksource factory resolves it at the point of use.
	os.Unsetenv("LINEAR_API_KEY")

	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	ws := merged.Governor.WorkSource
	if ws.Type != "linear" {
		t.Fatalf("work_source.type not restored from overlay: got %q want linear", ws.Type)
	}
	if ws.Linear.APIKey != "${LINEAR_API_KEY}" {
		t.Errorf("work_source.linear.api_key = %q, want the ${LINEAR_API_KEY} reference", ws.Linear.APIKey)
	}
	if len(ws.Linear.Teams) != 1 || ws.Linear.Teams[0].Key != "ENG" || ws.Linear.Teams[0].Repo != "testorg/repo1" {
		t.Errorf("work_source.linear.teams not restored: %+v", ws.Linear.Teams)
	}
	if len(ws.Linear.HoldLabels) != 1 || ws.Linear.HoldLabels[0] != "blocked" {
		t.Errorf("work_source.linear.hold_labels not restored: %v", ws.Linear.HoldLabels)
	}

	// An overlay with no work source must not clobber one set in the seed.
	seedWS := seed + `
governor:
  work_source:
    type: github_projects
    github_projects:
      project_number: 7
`
	if err := os.WriteFile(seedPath, []byte(seedWS), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayPath, []byte("project:\n  org: testorg\n  repos: [repo1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err = LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay (empty overlay): %v", err)
	}
	if merged.Governor.WorkSource.Type != "github_projects" || merged.Governor.WorkSource.GitHubProjects.ProjectNumber != 7 {
		t.Errorf("seed work source clobbered by empty overlay: %+v", merged.Governor.WorkSource)
	}
}

// TestRedactedForPersist_WorkSourceCredentials: a key loaded from
// `api_key: ${LINEAR_API_KEY}` is the real secret in memory; the persisted copy
// (seed rewrite and dashboard overlay) must fold it back into the reference.
func TestRedactedForPersist_WorkSourceCredentials(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_api_supersecretvalue0123456789")
	t.Setenv("JIRA_API_TOKEN", "atlassian_supersecrettoken0123456789")
	cfg := &Config{}
	cfg.Governor.WorkSource.Type = "linear"
	cfg.Governor.WorkSource.Linear.APIKey = "lin_api_supersecretvalue0123456789"
	cfg.Governor.WorkSource.Jira.APIToken = "atlassian_supersecrettoken0123456789"

	red := cfg.redactedForPersist()
	if got := red.Governor.WorkSource.Linear.APIKey; got != "${LINEAR_API_KEY}" {
		t.Errorf("linear.api_key persisted as %q, want ${LINEAR_API_KEY}", got)
	}
	if got := red.Governor.WorkSource.Jira.APIToken; got != "${JIRA_API_TOKEN}" {
		t.Errorf("jira.api_token persisted as %q, want ${JIRA_API_TOKEN}", got)
	}
	// The in-memory config keeps the resolved value.
	if strings.HasPrefix(cfg.Governor.WorkSource.Linear.APIKey, "${") {
		t.Errorf("redactedForPersist mutated the live config: %q", cfg.Governor.WorkSource.Linear.APIKey)
	}
}
