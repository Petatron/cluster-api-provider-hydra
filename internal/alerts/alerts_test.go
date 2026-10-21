/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package alerts holds only tests: it checks the PrometheusRule this provider
// ships in config/prometheus/alerts.yaml.
package alerts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	rulesPath = "../../config/prometheus/alerts.yaml"
	testsPath = "../../config/prometheus/alerts_test.yaml"
	docsPath  = "../../docs/alerts.md"
)

type rule struct {
	Alert       string            `json:"alert,omitempty"`
	Record      string            `json:"record,omitempty"`
	Expr        string            `json:"expr"`
	For         string            `json:"for,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type group struct {
	Name     string `json:"name"`
	Interval string `json:"interval,omitempty"`
	Rules    []rule `json:"rules"`
}

// prometheusRule is decoded strictly, so a misspelt field in the rules file
// fails here rather than being dropped by the API server.
type prometheusRule struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   map[string]any `json:"metadata"`
	Spec       struct {
		Groups []group `json:"groups"`
	} `json:"spec"`
}

func load(t *testing.T) prometheusRule {
	t.Helper()

	data, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	var pr prometheusRule
	if err := yaml.UnmarshalStrict(data, &pr); err != nil {
		t.Fatalf("%s: %v", rulesPath, err)
	}
	if pr.Kind != "PrometheusRule" || len(pr.Spec.Groups) == 0 {
		t.Fatalf("%s: want a PrometheusRule with groups, got kind %q and %d groups", rulesPath, pr.Kind, len(pr.Spec.Groups))
	}
	return pr
}

// anchor is the heading id GitHub renders for a markdown heading.
func anchor(heading string) string {
	return strings.ToLower(strings.Join(strings.Fields(heading), "-"))
}

// TestAlertsAreRoutableAndDocumented holds every alert to what a person paged
// at night needs: a severity Alertmanager can route on, a summary, a
// description, and a runbook link that lands on a section that exists.
func TestAlertsAreRoutableAndDocumented(t *testing.T) {
	pr := load(t)

	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	headings := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#{2,3} (.+)$`).FindAllStringSubmatch(string(docs), -1) {
		headings[anchor(m[1])] = true
	}

	const runbookBase = "https://github.com/Petatron/cluster-api-provider-hydra/blob/main/docs/alerts.md#"
	seen := map[string]bool{}
	for _, g := range pr.Spec.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" {
				continue
			}
			if !strings.HasPrefix(r.Alert, "Hydra") {
				t.Errorf("%s: alert names start with Hydra so one Alertmanager route matches them all", r.Alert)
			}
			if seen[r.Alert] {
				t.Errorf("%s: defined twice", r.Alert)
			}
			seen[r.Alert] = true
			if s := r.Labels["severity"]; s != "warning" && s != "critical" {
				t.Errorf("%s: severity %q, want warning or critical", r.Alert, s)
			}
			for _, a := range []string{"summary", "description", "runbook_url"} {
				if strings.TrimSpace(r.Annotations[a]) == "" {
					t.Errorf("%s: missing annotation %s", r.Alert, a)
				}
			}
			url := r.Annotations["runbook_url"]
			if !strings.HasPrefix(url, runbookBase) {
				t.Errorf("%s: runbook_url %q does not point into docs/alerts.md", r.Alert, url)
			} else if a := strings.TrimPrefix(url, runbookBase); !headings[a] {
				t.Errorf("%s: runbook_url anchor #%s has no heading in docs/alerts.md", r.Alert, a)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no alerts found")
	}
}

// TestEveryAlertIsUnitTested fails when an alert has no promtool case that
// expects it to fire. A rule nobody has seen fire is a rule nobody knows works.
func TestEveryAlertIsUnitTested(t *testing.T) {
	pr := load(t)

	data, err := os.ReadFile(testsPath)
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		RuleFiles []string `json:"rule_files"`
		Tests     []struct {
			AlertRuleTest []struct {
				Alertname string `json:"alertname"`
				ExpAlerts []any  `json:"exp_alerts"`
			} `json:"alert_rule_test"`
		} `json:"tests"`
	}
	if err := yaml.Unmarshal(data, &suite); err != nil {
		t.Fatalf("%s: %v", testsPath, err)
	}
	if len(suite.RuleFiles) != 1 || suite.RuleFiles[0] != "alerts.unit.rules.yaml" {
		t.Errorf("%s: rule_files must be [alerts.unit.rules.yaml], the copy TestAlertsWithPromtool writes", testsPath)
	}
	fires := map[string]bool{}
	for _, tc := range suite.Tests {
		for _, a := range tc.AlertRuleTest {
			if len(a.ExpAlerts) > 0 {
				fires[a.Alertname] = true
			}
		}
	}
	for _, g := range pr.Spec.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" && !fires[r.Alert] {
				t.Errorf("%s: no case in %s expects it to fire", r.Alert, testsPath)
			}
		}
	}
}

// TestAlertsWithPromtool runs promtool's own checks and the unit tests in
// config/prometheus/alerts_test.yaml against the rules. It needs promtool, so
// it runs only when PROMTOOL names one; `make test-alerts` downloads a pinned
// promtool and sets it, and CI runs that target.
func TestAlertsWithPromtool(t *testing.T) {
	promtool := os.Getenv("PROMTOOL")
	if promtool == "" {
		t.Skip("PROMTOOL is not set; run `make test-alerts`")
	}
	pr := load(t)

	// promtool reads plain rule files, which are a PrometheusRule's spec. It
	// lints the rules exactly as shipped, templates included. The unit tests
	// read a copy without annotations: promtool compares an alert's
	// annotations in full, and restating every description in the tests would
	// test the prose rather than the expressions. TestAlertsAreRoutableAndDocumented
	// covers the annotations instead.
	dir := t.TempDir()
	write := func(name string, v any) {
		t.Helper()
		data, err := yaml.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("alerts.rules.yaml", pr.Spec)
	for gi := range pr.Spec.Groups {
		for ri := range pr.Spec.Groups[gi].Rules {
			pr.Spec.Groups[gi].Rules[ri].Annotations = nil
		}
	}
	write("alerts.unit.rules.yaml", pr.Spec)
	tests, err := os.ReadFile(testsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alerts_test.yaml"), tests, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"check", "rules", "--lint=all", "--lint-fatal", "alerts.rules.yaml"},
		{"test", "rules", "alerts_test.yaml"},
	} {
		cmd := exec.Command(promtool, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("promtool %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}
