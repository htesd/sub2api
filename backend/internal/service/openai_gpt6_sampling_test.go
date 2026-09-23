//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// none 档（嵌套或顶层兼容写法）保留采样参数；其他档位删掉；Astra 与 5.6 不受这个函数影响。
func TestNormalizeGPT6ResponsesSampling(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		body     string
		stripped bool
	}{
		{"nested none keeps", "gpt-6-sol", `{"reasoning":{"effort":"none"},"temperature":0.2}`, false},
		{"top-level none keeps", "gpt-6-sol", `{"reasoning_effort":"none","temperature":0.2}`, false},
		{"top-level none upper keeps", "gpt-6-luna", `{"reasoning_effort":" NONE ","temperature":0.2}`, false},
		{"default effort strips", "gpt-6-sol", `{"temperature":0.2,"top_p":0.9}`, true},
		{"high strips", "gpt-6-luna-high", `{"reasoning":{"effort":"high"},"temperature":0.2}`, true},
		{"astra untouched", "gpt-6-astra", `{"temperature":0.2}`, false},
		{"gpt-5.6 untouched", "gpt-5.6-sol", `{"temperature":0.2}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := normalizeGPT6ResponsesSampling([]byte(tc.body), tc.model)
			require.NoError(t, err)
			require.Equal(t, tc.stripped, changed)
			require.Equal(t, !tc.stripped, gjson.GetBytes(out, "temperature").Exists())
		})
	}
}
