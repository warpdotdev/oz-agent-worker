package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestOzLifecycleHooksContextStrictUnmarshal(t *testing.T) {
	valid := fmt.Sprintf(`{
		"required": true,
		"supported_payload_schema_versions": ["%s"],
		"project_trust": [{
			"git_root": "/workspace/repo",
			"config_path": "/workspace/repo/.warp/hooks.json",
			"sha256": "%s"
		}]
	}`, OzHookPayloadSchemaV1, strings.Repeat("a", 64))

	var context OzLifecycleHooksContext
	if err := json.Unmarshal([]byte(valid), &context); err != nil {
		t.Fatalf("valid context rejected: %v", err)
	}
	if !context.Required || len(context.ProjectTrust) != 1 {
		t.Fatalf("unexpected decoded context: %+v", context)
	}

	tests := []struct {
		name string
		json string
	}{
		{
			name: "unknown context field",
			json: strings.Replace(valid, `"required": true`, `"required": true, "unknown": true`, 1),
		},
		{
			name: "unknown trust field",
			json: strings.Replace(valid, `"git_root":`, `"unknown": true, "git_root":`, 1),
		},
		{
			name: "required false",
			json: strings.Replace(valid, `"required": true`, `"required": false`, 1),
		},
		{
			name: "empty schema versions",
			json: strings.Replace(valid, `["warp.oz_hook.v1"]`, `[]`, 1),
		},
		{
			name: "unsupported schema version",
			json: strings.Replace(valid, OzHookPayloadSchemaV1, "warp.oz_hook.v2", 1),
		},
		{
			name: "missing project trust",
			json: fmt.Sprintf(`{"required":true,"supported_payload_schema_versions":["%s"]}`, OzHookPayloadSchemaV1),
		},
		{
			name: "non-canonical git root",
			json: strings.Replace(valid, `"/workspace/repo"`, `"workspace/repo"`, 1),
		},
		{
			name: "mismatched config path",
			json: strings.Replace(valid, `"/workspace/repo/.warp/hooks.json"`, `"/workspace/other/.warp/hooks.json"`, 1),
		},
		{
			name: "invalid sha256",
			json: strings.Replace(valid, strings.Repeat("a", 64), "not-a-hash", 1),
		},
		{
			name: "non-canonical sha256",
			json: strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var context OzLifecycleHooksContext
			if err := json.Unmarshal([]byte(test.json), &context); err == nil {
				t.Fatal("expected context to be rejected")
			}
		})
	}
}

func TestOzLifecycleHooksContextLimits(t *testing.T) {
	context := OzLifecycleHooksContext{
		Required:                       true,
		SupportedPayloadSchemaVersions: []string{OzHookPayloadSchemaV1},
		ProjectTrust:                   make([]OzLifecycleHookTrustRecord, MaxOzLifecycleHookTrustRecords+1),
	}
	if err := context.Validate(); err == nil {
		t.Fatal("expected trust record limit to be enforced")
	}

	oversized := fmt.Sprintf(
		`{"required":true,"supported_payload_schema_versions":["%s"],"project_trust":[],"padding":"%s"}`,
		OzHookPayloadSchemaV1,
		strings.Repeat("x", MaxOzLifecycleHooksContextSize),
	)
	var decoded OzLifecycleHooksContext
	err := json.Unmarshal([]byte(oversized), &decoded)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}
