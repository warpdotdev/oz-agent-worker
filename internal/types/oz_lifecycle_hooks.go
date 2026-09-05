package types

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	OzHookPayloadSchemaV1          = "warp.oz_hook.v1"
	MaxOzLifecycleHooksContextSize = 64 << 10
	MaxOzLifecycleHookTrustRecords = 64
)

func (m *TaskAssignmentMessage) UnmarshalJSON(data []byte) error {
	type assignmentAlias TaskAssignmentMessage
	var decoded struct {
		*assignmentAlias
		OzLifecycleHooks json.RawMessage `json:"oz_lifecycle_hooks"`
	}
	decoded.assignmentAlias = (*assignmentAlias)(m)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	m.OzLifecycleHooks = nil
	m.ozLifecycleHooksError = nil
	if len(decoded.OzLifecycleHooks) == 0 || bytes.Equal(decoded.OzLifecycleHooks, []byte("null")) {
		return nil
	}
	var context OzLifecycleHooksContext
	if err := json.Unmarshal(decoded.OzLifecycleHooks, &context); err != nil {
		m.ozLifecycleHooksError = err
		return nil
	}
	m.OzLifecycleHooks = &context
	return nil
}

func (m *TaskAssignmentMessage) OzLifecycleHooksValidationError() error {
	if m == nil {
		return nil
	}
	return m.ozLifecycleHooksError
}

type OzLifecycleHooksContext struct {
	Required                       bool                         `json:"required"`
	SupportedPayloadSchemaVersions []string                     `json:"supported_payload_schema_versions"`
	ProjectTrust                   []OzLifecycleHookTrustRecord `json:"project_trust"`
}

type OzLifecycleHookTrustRecord struct {
	GitRoot    string `json:"git_root"`
	ConfigPath string `json:"config_path"`
	SHA256     string `json:"sha256"`
}

func (c *OzLifecycleHooksContext) UnmarshalJSON(data []byte) error {
	if len(data) > MaxOzLifecycleHooksContextSize {
		return fmt.Errorf("oz lifecycle hooks context exceeds %d bytes", MaxOzLifecycleHooksContextSize)
	}

	type contextAlias OzLifecycleHooksContext
	var decoded contextAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("invalid oz lifecycle hooks context: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return fmt.Errorf("invalid oz lifecycle hooks context: %w", err)
	}

	*c = OzLifecycleHooksContext(decoded)
	return c.Validate()
}

func (c *OzLifecycleHooksContext) Validate() error {
	if c == nil {
		return nil
	}
	if !c.Required {
		return fmt.Errorf("oz lifecycle hooks context must set required to true")
	}
	if len(c.SupportedPayloadSchemaVersions) == 0 {
		return fmt.Errorf("oz lifecycle hooks context requires a supported payload schema version")
	}
	for _, version := range c.SupportedPayloadSchemaVersions {
		if version != OzHookPayloadSchemaV1 {
			return fmt.Errorf("unsupported oz lifecycle hook payload schema version %q", version)
		}
	}
	if len(c.ProjectTrust) > MaxOzLifecycleHookTrustRecords {
		return fmt.Errorf("oz lifecycle hooks context has %d project trust records; maximum is %d", len(c.ProjectTrust), MaxOzLifecycleHookTrustRecords)
	}
	if c.ProjectTrust == nil {
		return fmt.Errorf("oz lifecycle hooks context requires project_trust")
	}
	for i, record := range c.ProjectTrust {
		if strings.TrimSpace(record.GitRoot) == "" {
			return fmt.Errorf("project trust record %d has an empty git_root", i)
		}
		if strings.TrimSpace(record.ConfigPath) == "" {
			return fmt.Errorf("project trust record %d has an empty config_path", i)
		}
		hash, err := hex.DecodeString(record.SHA256)
		if err != nil || len(hash) != 32 {
			return fmt.Errorf("project trust record %d has an invalid sha256", i)
		}
	}

	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal oz lifecycle hooks context: %w", err)
	}
	if len(data) > MaxOzLifecycleHooksContextSize {
		return fmt.Errorf("oz lifecycle hooks context exceeds %d bytes", MaxOzLifecycleHooksContextSize)
	}
	return nil
}

func (c *OzLifecycleHooksContext) MarshalForCLI() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal oz lifecycle hooks context: %w", err)
	}
	return string(data), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
