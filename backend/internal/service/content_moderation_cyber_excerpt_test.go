package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestCyberPolicyInputExcerpt(t *testing.T) {
	for _, tt := range []struct {
		protocol, body string
		want           []string
	}{
		{ContentModerationProtocolOpenAIResponses, `{"instructions":"SYSTEM_SECRET","previous_response_id":"resp_test","input":[{"role":"developer","content":"DEVELOPER_SECRET"},{"role":"user","content":"initial task"},{"role":"assistant","content":[{"type":"output_text","text":"analysis result"}]},{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test\",\"password\":\"quoted password with spaces\"}"},{"type":"function_call_output","output":"test failed"},{"role":"user","content":"继续"}]}`, []string{"initial task", "analysis result", "exec_command", "go test", "test failed", "继续", "previous_response_id"}},
		{ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"system","content":"SYSTEM_SECRET"},{"role":"user","content":"initial task"},{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},{"role":"tool","content":"read completed"}]}`, []string{"initial task", "read_file", "README.md", "read completed", "tool"}},
		{ContentModerationProtocolAnthropicMessages, `{"system":"SYSTEM_SECRET","messages":[{"role":"assistant","content":[{"type":"tool_use","name":"test_runner","input":{"command":"pytest","token":"ab cd"}}]},{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"tests failed"},{"type":"image","source":{"data":"IMAGE_SECRET"}}]}]}]}`, []string{"test_runner", "pytest", "tests failed", "tool_result"}},
		{ContentModerationProtocolOpenAIResponses, `{"input":[{"type":"reasoning","summary":[{"text":"REASONING_SECRET"}],"encrypted_content":"ENCRYPTED_SECRET"},{"type":"input_image","image_url":"IMAGE_SECRET"},{"type":"input_file","file_data":"FILE_SECRET"},{"type":"custom_tool_call","name":"apply_patch","input":"safe patch"},{"type":"custom_tool_call_output","output":"applied"}]}`, []string{"apply_patch", "safe patch", "applied"}},
		{ContentModerationProtocolOpenAIResponses, `{"input":"a simple request"}`, []string{"a simple request"}},
	} {
		t.Run(tt.protocol+tt.body[:min(20, len(tt.body))], func(t *testing.T) {
			out := cyberPolicyInputExcerpt(tt.protocol, []byte(tt.body))
			for _, want := range tt.want {
				require.Contains(t, out, want)
			}
			for _, secret := range []string{"SYSTEM_SECRET", "DEVELOPER_SECRET", "quoted password with spaces", "ab cd", "IMAGE_SECRET", "REASONING_SECRET", "ENCRYPTED_SECRET", "FILE_SECRET"} {
				require.NotContains(t, out, secret)
			}
			require.NotContains(t, out, "已截断")
			require.Contains(t, out, "非人工违规结论")
		})
	}
	require.Empty(t, cyberPolicyInputExcerpt(ContentModerationProtocolOpenAIResponses, []byte(`{broken`)))
	require.Empty(t, cyberPolicyInputExcerpt("unknown", []byte(`{"input":"private"}`)))
	require.Contains(t, cyberPolicyInputExcerpt(ContentModerationProtocolOpenAIResponses, []byte(`{"input":[{"type":"input_image"}]}`)), "无可保存")
}

func TestCyberPolicyInputExcerptBounds(t *testing.T) {
	items := []map[string]string{{"role": "user", "content": "OLD_HISTORY"}}
	for i := 0; i < 30; i++ {
		items = append(items, map[string]string{"role": "tool", "content": strings.Repeat("测", 5000)})
	}
	items = append(items, map[string]string{"role": "user", "content": "LATEST_MESSAGE"})
	body, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)
	out := cyberPolicyInputExcerpt(ContentModerationProtocolOpenAIResponses, body)
	require.LessOrEqual(t, utf8.RuneCountInString(out), maxCyberPolicyExcerptRunes)
	require.Contains(t, out, "LATEST_MESSAGE")
	require.Contains(t, out, "已截断")
	require.NotContains(t, out, "OLD_HISTORY")
	require.True(t, utf8.ValidString(out))
	body, err = json.Marshal(map[string]string{"input": strings.Repeat("x", 256*1024+1)})
	require.NoError(t, err)
	require.Contains(t, cyberPolicyInputExcerpt(ContentModerationProtocolOpenAIResponses, body), "字段过长")
}

func TestCyberAuditSecretRedaction(t *testing.T) {
	for _, input := range []string{
		`{"password":"sensitive value with spaces", "api_key":"short", "Authorization":"Bearer smallsecret"}`,
		"token='sensitive value' pwd=abc socks5://user:pass@proxy.invalid:3000",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nsensitive-key-data\n-----END OPENSSH PRIVATE KEY----- after",
		`{"private_key":"-----BEGIN RSA PRIVATE KEY-----\nsensitive-key-data\n-----END RSA PRIVATE KEY-----"}`,
		`data:image/png;base64,c2VjcmV0`,
		`{"password":"first\"escaped secret"}`,
	} {
		out := redactContentModerationSecrets(input)
		for _, secret := range []string{"sensitive", "short", "smallsecret", "abc", "proxy.invalid", "PRIVATE KEY", "c2VjcmV0", "escaped secret"} {
			require.NotContains(t, out, secret)
		}
		require.Contains(t, out, "[已脱敏]")
	}
}

func TestCyberPolicyCannotBecomeRetryableError(t *testing.T) {
	for _, errType := range []string{"rate_limit_error", "server_error", "authentication_error", "invalid_request_error"} {
		reason, fallback := classifyOpenAIWSErrorEventFromRaw("cyber_policy", errType, "rate limit exceeded; previous response not found")
		require.Equal(t, "cyber_policy", reason)
		require.False(t, fallback)
		require.False(t, isOpenAIWSRateLimitError("cyber_policy", errType, "rate limit exceeded"))
		require.Equal(t, http.StatusForbidden, openAIWSErrorHTTPStatusFromRaw("cyber_policy", errType))
	}
	_, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest,
		[]byte(`{"input":"test","prompt_cache_breakpoint":true}`),
		[]byte(`{"error":{"code":"cyber_policy","param":"prompt_cache_breakpoint","message":"Unsupported parameter: 'prompt_cache_breakpoint'."}}`))
	require.NoError(t, err)
	require.False(t, changed)
}
