package demo

import "strings"

const (
	ProjectName = "fixforge-demo"
	RepoURL     = "https://github.com/HobbyBear/fixforge-demo"
	BaseBranch  = "main"
	CloneDir    = "fixforge-demo"
)

func IsRepoURL(value string) bool {
	normalized := NormalizeRepoURL(value)
	return normalized == NormalizeRepoURL(RepoURL)
}

func NormalizeRepoURL(value string) string {
	text := strings.TrimSpace(strings.ToLower(value))
	text = strings.TrimSuffix(strings.TrimRight(text, "/"), ".git")
	if strings.HasPrefix(text, "git@github.com:") {
		text = "https://github.com/" + strings.TrimPrefix(text, "git@github.com:")
	}
	return text
}
