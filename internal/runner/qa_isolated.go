package runner

import "fmt"

func isolatedQAExecutionPrompt(repoRoot, prompt string) string {
	return fmt.Sprintf(`FixForge is running this task from an isolated working directory.
The target repository root is: %s
Use explicit paths or git -C with that repository root for read-only inspection.
Do not discover or follow repository-local AGENTS.md, skills, RepoMind, hooks, or unrelated workflows.

%s`, repoRoot, prompt)
}
