package discovery

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/livepeer/node/discovery/shared"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

func discoverySchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schema, err := compiler.Compile("../docs/discovery.schema.json")
	require.NoError(t, err)
	return schema
}

func validateDiscoveryJSON(t *testing.T, schema *jsonschema.Schema, data []byte) error {
	t.Helper()
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&value))
	require.ErrorIs(t, decoder.Decode(&value), io.EOF)
	return schema.Validate(value)
}

func TestDiscoverySchema(t *testing.T) {
	schema := discoverySchema(t)
	const runner = `{"url":"https://orch.example/app","app":"example","observed_at":"2026-10-22T18:05:00.123+01:00"}`
	const metadata = `[{"nodes":[{"url":"https://orch.example","queried_at":"2026-10-22T18:05:00Z"}]}]`
	envelope := func(record string) string {
		return `[{"address":"https://orch.example/deployment","runners":[` + record + `]}]`
	}
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"empty", `[]`, true},
		{"empty runners", envelope(""), true},
		{"metadata", metadata, true},
		{"metadata with empty runners", `[{"nodes":[],"runners":[]}]`, true},
		{"minimal runner", envelope(runner), true},
		{"IPv6 with port", strings.ReplaceAll(envelope(runner), "orch.example", "[::1]:8935"), true},
		{"mixed-case schemes", strings.ReplaceAll(envelope(runner), "https://", "hTtPs://"), true},
		{"mixed-case node scheme", strings.ReplaceAll(metadata, "https://", "HtTp://"), true},
		{"runner URL components", envelope(strings.ReplaceAll(runner, "https://orch.example/app", "https://user:pass@orch.example/app?x=1#part")), true},
		{"unknown properties", envelope(strings.TrimSuffix(runner, "}") + `,"gpu.name":null,"versıon":null,"extension":{"count":9007199254740993}}`), true},
		{"extensible price", envelope(strings.TrimSuffix(runner, "}") + `,"mode":"future","price_info":{"price":123456789012345678901234567890,"price_usd":0.123456789012345678901234567890123456,"currency":"credits","unit":"tokens","extension":true}}`), true},
		{"USD price omitted", envelope(strings.TrimSuffix(runner, "}") + `,"price_info":{"price":1,"currency":"wei","unit":"fixed"}}`), true},
		{"price string", envelope(strings.TrimSuffix(runner, "}") + `,"price_info":{"price":"1"}}`), false},
		{"USD price string", envelope(strings.TrimSuffix(runner, "}") + `,"price_info":{"price":1,"price_usd":"0.1"}}`), false},
		{"capacity string", envelope(strings.TrimSuffix(runner, "}") + `,"capacity":"4"}`), false},
		{"fractional capacity", envelope(strings.TrimSuffix(runner, "}") + `,"capacity_available":1.5}`), false},
		{"integer tokens", envelope(strings.TrimSuffix(runner, "}") + `,"capacity":4.0,"capacity_used":1e0,"capacity_available":9007199254740993}`), true},
		{"scaled integers", envelope(strings.TrimSuffix(runner, "}") + `,"capacity":1200.0e-2,"capacity_used":1.20E1,"capacity_available":-0.00e-100}`), true},
		{"version number", envelope(strings.TrimSuffix(runner, "}") + `,"version":1}`), false},
		{"null metadata", envelope(strings.TrimSuffix(runner, "}") + `,"metadata":null}`), false},
		{"null GPU", envelope(strings.TrimSuffix(runner, "}") + `,"gpu":null}`), false},
		{"GPU name number", envelope(strings.TrimSuffix(runner, "}") + `,"gpu":{"name":1}}`), false},
		{"fractional VRAM", envelope(strings.TrimSuffix(runner, "}") + `,"gpu":{"vram_mb":1.5}}`), false},
		{"null price", envelope(strings.TrimSuffix(runner, "}") + `,"price_info":{"price":null}}`), false},
		{"missing URL", envelope(strings.ReplaceAll(runner, `"url":"https://orch.example/app",`, "")), false},
		{"wrong URL field case", envelope(strings.ReplaceAll(runner, `"url":`, `"URL":`)), false},
		{"missing app", envelope(strings.ReplaceAll(runner, `"app":"example",`, "")), false},
		{"missing observation", envelope(`{"url":"https://orch.example/app","app":"example"}`), false},
		{"empty app", envelope(strings.ReplaceAll(runner, `"app":"example"`, `"app":""`)), false},
		{"invalid observation", envelope(strings.ReplaceAll(runner, "2026-10-22T18:05:00.123+01:00", "yesterday")), false},
		{"comma observation fraction", envelope(strings.ReplaceAll(runner, "00.123", "00,123")), false},
		{"invalid observation offset", envelope(strings.ReplaceAll(runner, "+01:00", "+24:00")), false},
		{"invalid query time", `[{"nodes":[{"url":"https://orch.example","queried_at":"yesterday"}]}]`, false},
		{"missing query time", `[{"nodes":[{"url":"https://orch.example"}]}]`, false},
		{"missing node URL", `[{"nodes":[{"queried_at":"2026-10-22T18:05:00Z"}]}]`, false},
		{"metadata with address", `[{"nodes":[],"address":"https://orch.example"}]`, false},
		{"metadata with runners", `[{"nodes":[],"runners":[` + runner + `]}]`, false},
		{"metadata with non-array runners", `[{"nodes":[],"runners":{}}]`, false},
		{"missing runners", `[{"address":"https://orch.example"}]`, false},
		{"non-HTTP address", strings.ReplaceAll(envelope(runner), "https://orch.example/deployment", "ftp://orch.example"), false},
		{"non-HTTP runner URL", envelope(strings.ReplaceAll(runner, "https://orch.example/app", "ftp://orch.example/app")), false},
		{"address credentials", strings.ReplaceAll(envelope(runner), "https://orch.example/deployment", "https://user:pass@orch.example/deployment"), false},
		{"address query", strings.ReplaceAll(envelope(runner), "https://orch.example/deployment", "https://orch.example/deployment?x=1"), false},
		{"address empty query", strings.ReplaceAll(envelope(runner), "https://orch.example/deployment", "https://orch.example/deployment?"), false},
		{"address fragment", strings.ReplaceAll(envelope(runner), "https://orch.example/deployment", "https://orch.example/deployment#part"), false},
		{"node credentials", strings.ReplaceAll(metadata, "https://orch.example", "https://user:pass@orch.example"), false},
		{"node query", strings.ReplaceAll(metadata, "https://orch.example", "https://orch.example?x=1"), false},
		{"node fragment", strings.ReplaceAll(metadata, "https://orch.example", "https://orch.example#part"), false},
		{"hostless address", strings.ReplaceAll(envelope(runner), "https://orch.example/deployment", "https://:8935/deployment"), false},
		{"hostless node", strings.ReplaceAll(metadata, "https://orch.example", "https://:8935"), false},
		{"userinfo-only runner", envelope(strings.ReplaceAll(runner, "https://orch.example/app", "https://user:pass@/app")), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateDiscoveryJSON(t, schema, []byte(test.body))
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			entries, err := shared.Decode[discoveryRunner]([]byte(test.body), time.Now())
			if test.valid {
				require.NoError(t, err)
			}
			if err == nil {
				body, err := json.Marshal(entries)
				require.NoError(t, err)
				require.NoError(t, validateDiscoveryJSON(t, schema, body))
			}
		})
	}
}

func TestDiscoveryRunnerValidation(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		valid        bool
	}{
		{"large integer exponent", `"gpu":{"vram_mb":1e1000000}`, true},
		{"overflow integer exponent", `"gpu":{"vram_mb":1e999999999999999999999}`, true},
		{"large fractional exponent", `"gpu":{"vram_mb":1e-1000000}`, false},
		{"overflow fractional exponent", `"gpu":{"vram_mb":1e-999999999999999999999}`, false},
		{"zero with overflow exponent", `"gpu":{"vram_mb":0e-999999999999999999999}`, true},
		{"case alias", `"URL":"https://other.example/app"`, false},
		{"Unicode case alias", `"capacity_used":1,"capacity_uſed":9`, false},
		{"nested Unicode case alias", `"price_info":{"price_usd":0.1,"price_uſd":0.9}`, false},
		{"optional field case", `"Mode":"persistent"`, false},
		{"duplicate field", `"capacity":1,"capacity":2`, false},
		{"duplicate extension field", `"extension":{"x":1,"x":2}`, false},
		{"invalid Unicode", `"extension":"\ud800"`, false},
		{"invalid UTF-8", `"extension":"` + "\xff" + `"`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := shared.Decode[discoveryRunner]([]byte(`[{"address":"https://orch.example","runners":[{"url":"https://orch.example/app","app":"test","observed_at":"2000-01-01T00:00:00Z",`+test.fields+`}]}]`), time.Now())
			require.Equal(t, test.valid, err == nil)
		})
	}
}

func TestDiscoverySchemaHTTPExample(t *testing.T) {
	doc, err := os.ReadFile("../docs/http-api.md")
	require.NoError(t, err)
	_, section, ok := strings.Cut(string(doc), "## Discovery HTTP\n")
	require.True(t, ok, "Discovery HTTP section must exist")
	section, _, _ = strings.Cut(section, "\n## ")
	_, block, ok := strings.Cut(section, "```json\n")
	require.True(t, ok, "Discovery HTTP must contain a JSON example")
	example, _, ok := strings.Cut(block, "\n```")
	require.True(t, ok, "JSON example must have a closing fence")
	require.NoError(t, validateDiscoveryJSON(t, discoverySchema(t), []byte(example)))
}
