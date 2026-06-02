package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSkillMarkdown(t *testing.T) {
	tests := []struct {
		name           string
		content        string
		wantName       string
		wantDesc       string
		wantBasePrompt string
		wantErr        bool
		wantErrText    string
	}{
		{
			name: "valid frontmatter with name and description",
			content: `---
name: deploy
description: Deploy to production
---
Do the deployment.
`,
			wantName:       "deploy",
			wantDesc:       "Deploy to production",
			wantBasePrompt: "Do the deployment.",
		},
		{
			name: "valid frontmatter name only",
			content: `---
name: my-skill
---
Some instructions.
`,
			wantName:       "my-skill",
			wantDesc:       "",
			wantBasePrompt: "Some instructions.",
		},
		{
			name:        "missing frontmatter",
			content:     "Just some markdown without frontmatter.",
			wantErr:     true,
			wantErrText: "missing YAML front matter",
		},
		{
			name: "empty name",
			content: `---
description: something
---
body
`,
			wantErr:     true,
			wantErrText: "missing skill name in front matter",
		},
		{
			name: "whitespace-only name",
			content: `---
name: "  "
---
body
`,
			wantErr:     true,
			wantErrText: "missing skill name in front matter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, desc, basePrompt, err := parseSkillMarkdown(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrText != "" && err.Error() != tt.wantErrText {
					t.Errorf("error = %q, want %q", err.Error(), tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if desc != tt.wantDesc {
				t.Errorf("description = %q, want %q", desc, tt.wantDesc)
			}
			if basePrompt != tt.wantBasePrompt {
				t.Errorf("basePrompt = %q, want %q", basePrompt, tt.wantBasePrompt)
			}
		})
	}
}

func TestScanSkillsDirs(t *testing.T) {
	ctx := context.Background()

	// Create a temp directory structure:
	// dir/
	//   skill-a/SKILL.md  (valid)
	//   skill-b/SKILL.md  (valid)
	//   not-a-dir.txt     (file, should be skipped)
	//   skill-c/           (no SKILL.md, should be skipped)
	//   skill-d/SKILL.md  (invalid frontmatter, should be skipped)
	dir := t.TempDir()

	writeSkill(t, dir, "skill-a", `---
name: skill-a
description: First skill
---
Do something.
`)
	writeSkill(t, dir, "skill-b", `---
name: skill-b
description: Second skill
---
Do something else.
`)

	// File at top level (not a directory)
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Directory without SKILL.md
	if err := os.MkdirAll(filepath.Join(dir, "skill-c"), 0755); err != nil {
		t.Fatal(err)
	}

	// Directory with invalid SKILL.md
	writeSkill(t, dir, "skill-d", "no frontmatter here")

	skills := scanSkillsDirs(ctx, []string{dir})

	if len(skills) != 2 {
		t.Fatalf("got %d skills, want 2", len(skills))
	}

	// Sort isn't guaranteed, so check by name
	nameSet := map[string]bool{}
	for _, s := range skills {
		nameSet[s.Name] = true
		if s.Path == "" {
			t.Errorf("skill %q has empty path", s.Name)
		}
	}
	if !nameSet["skill-a"] {
		t.Error("missing skill-a")
	}
	if !nameSet["skill-b"] {
		t.Error("missing skill-b")
	}
}

func TestScanSkillsDirsNonexistentDir(t *testing.T) {
	ctx := context.Background()
	skills := scanSkillsDirs(ctx, []string{"/nonexistent/path/12345"})
	if len(skills) != 0 {
		t.Errorf("expected 0 skills for nonexistent dir, got %d", len(skills))
	}
}

func TestScanSkillsDirsDedup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	writeSkill(t, dir, "my-skill", `---
name: my-skill
description: A skill
---
Body.
`)

	// Pass same directory twice
	skills := scanSkillsDirs(ctx, []string{dir, dir})
	if len(skills) != 1 {
		t.Errorf("expected 1 skill after dedup, got %d", len(skills))
	}
}

func TestScanSkillsDirsEmpty(t *testing.T) {
	ctx := context.Background()
	skills := scanSkillsDirs(ctx, nil)
	if len(skills) != 0 {
		t.Errorf("expected 0 skills for nil dirs, got %d", len(skills))
	}
	skills = scanSkillsDirs(ctx, []string{})
	if len(skills) != 0 {
		t.Errorf("expected 0 skills for empty dirs, got %d", len(skills))
	}
}

func writeSkill(t *testing.T, baseDir, skillName, content string) {
	t.Helper()
	skillDir := filepath.Join(baseDir, skillName)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
