package runner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestGitHubReleaseAtUsesReleaseRedirect(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "fixforge-client" {
			http.Error(w, "missing User-Agent", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/HobbyBear/fixforge-client/releases/tag/v0.1.12", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	version, err := latestGitHubReleaseAt("hobbybear/fixforge-client", server.URL+"/releases/latest", nil)
	if err != nil {
		t.Fatalf("latestGitHubReleaseAt() error = %v", err)
	}
	if version != "v0.1.12" {
		t.Fatalf("latestGitHubReleaseAt() = %q, want v0.1.12", version)
	}
}

func TestLatestGitHubReleaseAtRejectsUnexpectedRedirect(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other/repository/releases/tag/v9.9.9", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	_, err := latestGitHubReleaseAt("HobbyBear/fixforge-client", server.URL+"/releases/latest", nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected location") {
		t.Fatalf("latestGitHubReleaseAt() error = %v, want unexpected location", err)
	}
}

func TestLatestGitHubReleaseAtRejectsNonRedirect(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	_, err := latestGitHubReleaseAt("HobbyBear/fixforge-client", server.URL+"/releases/latest", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("latestGitHubReleaseAt() error = %v, want HTTP 403", err)
	}
}
