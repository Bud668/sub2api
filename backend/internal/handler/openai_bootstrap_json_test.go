package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexBootstrapJSONBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		output    string
		normalize func([]byte) ([]byte, bool)
	}{
		{"automation_update", "codex_app", `<heartbeat><automation_id>wiki</automation_id></heartbeat>`, normalizeCodexAutomationBootstrap},
		{"create_thread", "codex_app", delegationEnvelope, normalizeCodexDelegationBootstrap},
		{"send_message_to_thread", "codex_tui", delegationEnvelope, normalizeCodexDelegationBootstrap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := `{"type":"function_call_output","namespace":"` + tc.namespace + `","name":"` + tc.name + `","output":` + mustJSON(t, tc.output) + `}`
			body := `{"model":"gpt-5","input":[` + item + `]}`
			t.Run("escaped keys and values", func(t *testing.T) {
				escaped := strings.NewReplacer(
					`"input"`, `"\u0069nput"`, `"type"`, `"\u0074ype"`,
					`"namespace"`, `"\u006eamespace"`, `"name"`, `"\u006eame"`,
					`"output"`, `"\u006futput"`, `"function_call_output"`, `"\u0066unction_call_output"`,
					`"codex_`, `"\u0063odex_`, `"`+tc.name+`"`, fmt.Sprintf(`"\u%04x%s"`, tc.name[0], tc.name[1:]),
				).Replace(body)
				got, changed := tc.normalize([]byte(escaped))
				require.True(t, changed)
				require.Equal(t, tc.output, gjson.GetBytes(got, "input.0.content.0.text").String())
			})
			t.Run("later matching item", func(t *testing.T) {
				input := `{"model":"gpt-5","input":[{"type":"message","role":"user","name":"` + tc.name + `","content":"before"},` + item + `]}`
				got, changed := tc.normalize([]byte(input))
				require.True(t, changed)
				require.Equal(t, tc.output, gjson.GetBytes(got, "input.1.content.0.text").String())
			})
			for name, input := range map[string]string{
				"duplicate input":        strings.Replace(body, `"input":`, `"input":[],"input":`, 1),
				"escaped duplicate name": strings.Replace(body, `"name":`, `"\u006eame":"other","name":`, 1),
				"nested duplicate":       strings.Replace(body, `"model":`, `"metadata":{"a":{"key":1,"key":2}},"model":`, 1),
				"trailing JSON":          body + `{}`,
				"truncated JSON":         body[:len(body)-1],
				"name only in text":      `{"input":[{"role":"user","content":` + mustJSON(t, item) + `}]}`,
			} {
				t.Run(name, func(t *testing.T) {
					got, changed := tc.normalize([]byte(input))
					require.False(t, changed)
					require.Equal(t, input, string(got))
				})
			}
			unchanged, changed := tc.normalize([]byte(`{"input":"ordinary conversation"}`))
			require.False(t, changed)
			require.Equal(t, `{"input":"ordinary conversation"}`, string(unchanged))
		})
	}
}

// Synthetic long histories only: no production request content or upstream calls.
func BenchmarkNormalizeCodexBootstrapLongContext(b *testing.B) {
	for _, turns := range []int{16, 256, 1024} {
		b.Run(fmt.Sprintf("%dTurns", turns), func(b *testing.B) {
			text, err := json.Marshal(strings.Repeat("ordinary context ", 256))
			require.NoError(b, err)
			item := `{"type":"message","role":"user","content":` + string(text) + `},{"type":"function_call","call_id":"call-1","name":"exec_command","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","namespace":"codex_app","name":"exec_command","output":"done"}`
			body := []byte(`{"model":"gpt-5","input":[` + strings.Repeat(item+",", turns-1) + item + `]}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, automation := normalizeCodexAutomationBootstrap(body)
				got, delegation := normalizeCodexDelegationBootstrap(got)
				if automation || delegation || !bytes.Equal(body, got) {
					b.Fatal("ordinary history was modified")
				}
			}
			b.ReportMetric(float64(len(body)), "body-B")
		})
	}
}
