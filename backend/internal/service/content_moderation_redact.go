package service

import (
	"regexp"
	"strings"
)

var contentModerationSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>，。；、]+`),
	regexp.MustCompile(`(?i)\b((?:api[_-]?key|apikey|access[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|token|session|cookie|set[_-]?cookie|authorization|bearer|password|passwd|pwd|secret|client[_-]?secret|private[_-]?key)\s*[:=]\s*)(["']?)[^"'\s,;，。；、]{6,}`),
	regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?i)\b(?:sk|sk-proj|sk-ant|sess|rk|pk|ak|api|key|token|secret)[_-][A-Za-z0-9._~+/=-]{12,}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9_-]{48,}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9+/]{48,}={0,2}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`),
}

var contentModerationPrivateKeyPattern = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
var contentModerationQuotedSecretPattern = regexp.MustCompile(`(?i)((?:["']|\b)(?:api[_ -]?key|apikey|access[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|token|cookie|set[_-]?cookie|authorization|password|passwd|pwd|secret|client[_-]?secret|private[_-]?key)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;，。；、]+)`)
var contentModerationInlineDataPattern = regexp.MustCompile(`(?i)\bdata:[^\s"'<>]+`)

func redactContentModerationSecrets(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	out := contentModerationPrivateKeyPattern.ReplaceAllString(text, `[已脱敏]`)
	out = contentModerationQuotedSecretPattern.ReplaceAllString(out, `${1}"[已脱敏]"`)
	out = contentModerationInlineDataPattern.ReplaceAllString(out, `[已脱敏]`)
	for idx, pattern := range contentModerationSecretPatterns {
		switch idx {
		case 1:
			out = pattern.ReplaceAllString(out, `${1}${2}[已脱敏]`)
		case 2:
			out = pattern.ReplaceAllString(out, `${1}[已脱敏]`)
		default:
			out = pattern.ReplaceAllString(out, `[已脱敏]`)
		}
	}
	return out
}
