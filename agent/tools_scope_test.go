package agent

import (
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// coverage.scope is built from task_scope rows, where a rule can appear both in
// the task's own scope and in the scope of a directly related source task. These
// tests pin the shaping rules (dedup, provenance, caps) without needing Postgres.

func TestTaskScopeOverviewCollapsesDuplicateRules(t *testing.T) {
	const taskID, sourceTaskID int64 = 11, 22
	companyID := int64(7)
	rows := []db.TaskScope{
		{TaskID: taskID, Kind: "company", CompanyID: &companyID, CompanyName: "Acme", Source: "manual"},
		{TaskID: sourceTaskID, Kind: "company", CompanyID: &companyID, CompanyName: "Acme", Source: "manual"},
		{TaskID: taskID, Kind: "root_domain", Domain: "example.com", Source: "agent"},
		{TaskID: sourceTaskID, Kind: "root_domain", Domain: "EXAMPLE.com", Source: "agent"},
		{TaskID: taskID, Kind: "ip", Net: "10.0.0.1/32", Source: "auto"},
		{TaskID: taskID, Kind: "cidr", Net: "10.0.0.1/32", Source: "agent"},
		{TaskID: sourceTaskID, Kind: "keyword", Value: "acme cloud", Source: "agent"},
	}
	tools := &ToolSet{taskID: taskID}
	scope, omitted := tools.taskScopeOverview(rows)
	if omitted != 0 {
		t.Fatalf("omitted=%d, want 0 for a small scope", omitted)
	}
	if len(scope) != 4 {
		t.Fatalf("scope rows=%d, want 4 (company, root_domain, ip, inherited keyword): %#v", len(scope), scope)
	}

	byKind := map[string]map[string]any{}
	for _, row := range scope {
		byKind[fmt.Sprint(row["kind"])] = row
	}
	company := byKind["company"]
	if company == nil || company["company_id"] != companyID || company["company_name"] != "Acme" {
		t.Fatalf("direct company row lost its identity: %#v", company)
	}
	// The direct row must win, so the inherited copy never re-labels it.
	if _, inherited := company["inherited"]; inherited {
		t.Fatalf("direct company row was labelled inherited: %#v", company)
	}
	if row := byKind["root_domain"]; row == nil || row["task_id"] != taskID {
		t.Fatalf("direct root_domain row lost: %#v", row)
	}
	if row := byKind["cidr"]; row != nil {
		t.Fatalf("ip and cidr for the same net should collapse, got both: %#v", scope)
	}
	keyword := byKind["keyword"]
	if keyword == nil || keyword["inherited"] != true || keyword["source_task_id"] != sourceTaskID {
		t.Fatalf("inherited keyword row needs provenance: %#v", keyword)
	}
}

func TestTaskScopeOverviewBoundsRowCount(t *testing.T) {
	const taskID int64 = 5
	rows := make([]db.TaskScope, 0, scopeRowsCap+12)
	for i := 0; i < scopeRowsCap+12; i++ {
		rows = append(rows, db.TaskScope{
			TaskID: taskID, Kind: "subdomain", Domain: fmt.Sprintf("h%d.example.com", i), Source: "auto",
		})
	}
	scope, omitted := (&ToolSet{taskID: taskID}).taskScopeOverview(rows)
	if len(scope) != scopeRowsCap || omitted != 12 {
		t.Fatalf("scope rows=%d omitted=%d, want %d/%d", len(scope), omitted, scopeRowsCap, 12)
	}
}

func TestTaskScopeOverviewKeepsCompanyWithoutRules(t *testing.T) {
	// No CompanyStore wired (or a company that was deleted): the row must still
	// report which company the task was scoped to, minus the expansion.
	const taskID int64 = 3
	companyID := int64(9)
	scope, _ := (&ToolSet{taskID: taskID}).taskScopeOverview([]db.TaskScope{
		{TaskID: taskID, Kind: "company", CompanyID: &companyID, CompanyName: "Gone Ltd"},
	})
	if len(scope) != 1 {
		t.Fatalf("scope=%#v, want the company row to survive", scope)
	}
	if scope[0]["company_id"] != companyID {
		t.Fatalf("company_id lost: %#v", scope[0])
	}
	if _, ok := scope[0]["company_scope"]; ok {
		t.Fatalf("company rules expanded without a store: %#v", scope[0])
	}
}

func TestApplyCompanyScopeUsesRawTextAndKeywords(t *testing.T) {
	e := map[string]any{}
	budget := &scopeExpansionBudget{rules: companyScopeRulesTotalCap, keywords: companyKeywordsTotalCap}
	applyCompanyScope(e, []db.ScopeRule{
		{Kind: "domain", Domain: "example.com", Raw: "example.com"},
		{Kind: "domain", Domain: "EXAMPLE.com", Raw: "EXAMPLE.com"}, // case-duplicate
		{Kind: "ip", Net: "10.0.0.1/32", Raw: "10.0.0.1"},
		{Kind: "icp", Value: "京icp备1号", Raw: "京 ICP 备 1 号"},
		{Kind: "keyword", Value: "scope company 1", Raw: "Scope Company 1"},
	}, budget)

	rules, _ := e["company_scope"].([]map[string]any)
	if len(rules) != 4 {
		t.Fatalf("company_scope=%#v, want 4 rules after dedup", e["company_scope"])
	}
	values := map[string]string{}
	for _, rule := range rules {
		values[fmt.Sprint(rule["kind"])] = fmt.Sprint(rule["value"])
	}
	// Raw is what the user typed: the planner needs the original text, not the
	// normalized matching value (ip would otherwise gain "/32", ICP its spaces).
	want := map[string]string{
		"domain":  "example.com",
		"ip":      "10.0.0.1",
		"icp":     "京 ICP 备 1 号",
		"keyword": "Scope Company 1",
	}
	for kind, value := range want {
		if values[kind] != value {
			t.Errorf("company_scope[%s]=%q, want %q", kind, values[kind], value)
		}
	}
	keywords, ok := e["company_keywords"].([]string)
	if !ok || len(keywords) != 1 || keywords[0] != "Scope Company 1" {
		t.Fatalf("company_keywords=%#v, want the raw keyword", e["company_keywords"])
	}
}

func TestApplyCompanyScopeEnforcesCaps(t *testing.T) {
	rules := make([]db.ScopeRule, 0, companyScopeRulesPerCompanyCap+5)
	for i := 0; i < companyScopeRulesPerCompanyCap+5; i++ {
		raw := fmt.Sprintf("h%d.example.com", i)
		rules = append(rules, db.ScopeRule{Kind: "domain", Domain: raw, Raw: raw})
	}
	e := map[string]any{}
	budget := &scopeExpansionBudget{rules: companyScopeRulesTotalCap, keywords: companyKeywordsTotalCap}
	applyCompanyScope(e, rules, budget)
	if got := len(e["company_scope"].([]map[string]any)); got != companyScopeRulesPerCompanyCap {
		t.Fatalf("per-company expansion=%d, want %d", got, companyScopeRulesPerCompanyCap)
	}
	if e["company_scope_truncated"] != 5 {
		t.Fatalf("company_scope_truncated=%v, want 5", e["company_scope_truncated"])
	}

	// The global budget (several companies in one overview) is enforced too.
	e = map[string]any{}
	budget = &scopeExpansionBudget{rules: 3, keywords: companyKeywordsTotalCap}
	applyCompanyScope(e, rules, budget)
	if got := len(e["company_scope"].([]map[string]any)); got != 3 {
		t.Fatalf("global-budget expansion=%d, want 3", got)
	}
	if e["company_scope_truncated"] == nil {
		t.Fatal("global budget truncation must be reported, not silently dropped")
	}
}

func TestTaskScopeOverviewDoesNotCollapseMalformedRows(t *testing.T) {
	// 载荷列全空的行属于数据异常（schema 下不该出现）。它们不能按 kind 互相折叠，
	// 否则"范围里少了几条"会静默消失。
	const taskID int64 = 8
	rows := []db.TaskScope{
		{ID: 101, TaskID: taskID, Kind: "company"},
		{ID: 102, TaskID: taskID, Kind: "company"},
		{ID: 103, TaskID: taskID, Kind: "icp"},
	}
	scope, omitted := (&ToolSet{taskID: taskID}).taskScopeOverview(rows)
	if omitted != 0 {
		t.Fatalf("omitted=%d, want 0", omitted)
	}
	if len(scope) != len(rows) {
		t.Fatalf("malformed rows collapsed: got %d, want %d", len(scope), len(rows))
	}
}
