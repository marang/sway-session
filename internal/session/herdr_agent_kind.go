package session

// ValidHerdrAgentKind reports whether value is one of Herdr's fixed supported
// interactive agent kinds. It is deliberately a closed vocabulary so broker
// input can never influence a command, API method, or source prefix.
func ValidHerdrAgentKind(value string) bool {
	switch value {
	case "agy", "amp", "claude", "cline", "codex", "copilot", "cursor",
		"devin", "droid", "gemini", "grok", "hermes", "kilo", "kimi",
		"kiro", "maki", "mastracode", "omp", "opencode", "pi", "qodercli", "qwen":
		return true
	default:
		return false
	}
}
