package worker

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"gopkg.in/yaml.v3"
)

var skillFrontMatterRegex = regexp.MustCompile(`(?ms)\A\s*---[ \t]*\r?\n(.*?)\r?\n---[ \t]*\r?\n?`)

type skillFrontMatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// scanSkillsDirs walks the configured skills directories and returns all valid skills found.
// Each directory should contain subdirectories with a SKILL.md file:
//
//	<dir>/skill-name/SKILL.md
//
// Skills with missing or unparseable frontmatter are skipped with a warning.
func scanSkillsDirs(ctx context.Context, dirs []string) []types.WorkerSkill {
	var skills []types.WorkerSkill
	seen := make(map[string]struct{}) // deduplicate by absolute path

	for _, dir := range dirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			log.Warnf(ctx, "Skipping skills dir %q: failed to resolve absolute path: %v", dir, err)
			continue
		}

		entries, err := os.ReadDir(absDir)
		if err != nil {
			log.Warnf(ctx, "Skipping skills dir %q: %v", absDir, err)
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			skillFile := filepath.Join(absDir, entry.Name(), "SKILL.md")
			skill, err := parseSkillFile(skillFile)
			if err != nil {
				log.Warnf(ctx, "Skipping skill at %q: %v", skillFile, err)
				continue
			}

			if _, exists := seen[skill.Path]; exists {
				continue
			}
			seen[skill.Path] = struct{}{}
			skills = append(skills, *skill)
		}
	}

	return skills
}

// parseSkillFile reads a SKILL.md file and extracts the skill metadata from its YAML frontmatter.
func parseSkillFile(path string) (*types.WorkerSkill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	name, description, basePrompt, err := parseSkillMarkdown(string(data))
	if err != nil {
		return nil, err
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	return &types.WorkerSkill{
		Name:        name,
		Description: description,
		Path:        absPath,
		BasePrompt:  basePrompt,
	}, nil
}

// parseSkillMarkdown extracts the name, description, and base prompt from a SKILL.md file.
func parseSkillMarkdown(content string) (string, string, string, error) {
	loc := skillFrontMatterRegex.FindStringSubmatchIndex(content)
	if len(loc) < 4 {
		return "", "", "", errMissingFrontMatter
	}

	frontMatterYaml := content[loc[2]:loc[3]]
	var fm skillFrontMatter
	if err := yaml.Unmarshal([]byte(frontMatterYaml), &fm); err != nil {
		return "", "", "", err
	}

	name := strings.TrimSpace(fm.Name)
	if name == "" {
		return "", "", "", errMissingSkillName
	}

	basePrompt := strings.TrimSpace(content[loc[1]:])
	return name, strings.TrimSpace(fm.Description), basePrompt, nil
}

type skillError string

func (e skillError) Error() string { return string(e) }

const (
	errMissingFrontMatter skillError = "missing YAML front matter"
	errMissingSkillName   skillError = "missing skill name in front matter"
)
