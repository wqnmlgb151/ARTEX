package agent

// ChildSecretOverrides clears credentials owned by the API process when the
// tool SDK layers subprocess environments. Explicit MCP-specific credentials may
// still be configured separately. This is hygiene, not a process security boundary.
func ChildSecretOverrides() []string {
	return []string{
		"ARTEX_PG_DSN=", "POSTGRES_PASSWORD=", "PGPASSWORD=", "PGPASSFILE=",
		"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "ARTEX_UPDATE_GITHUB_TOKEN=",
		"ARTEX_CONFIG=", "ARTEX_AUTH_DIR=",
	}
}
