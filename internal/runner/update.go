package runner

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const clientBinaryName = "fixforge-client"

type httpStatusError struct {
	url    string
	status int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("GET %s failed: HTTP %d", e.url, e.status)
}

// DoUpdate replaces the current fixforge-client binary from a GitHub release.
func DoUpdate(args []string, currentVersion, defaultRepo string) error {
	opts := updateOptions{
		version:        "latest",
		repo:           strings.TrimSpace(defaultRepo),
		restartService: true,
	}
	if opts.repo == "" {
		opts.repo = "HobbyBear/fixforge-client"
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.StringVar(&opts.version, "version", opts.version, "release version, for example v0.1.0")
	fs.StringVar(&opts.repo, "repo", opts.repo, "GitHub repository in owner/name form")
	fs.StringVar(&opts.installPath, "install-path", "", "full binary path to replace; default is current executable")
	fs.StringVar(&opts.installDir, "install-dir", "", "directory to install fixforge-client into")
	fs.BoolVar(&opts.restartService, "restart-service", opts.restartService, "stop and restart the local service when it is installed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		opts.version = fs.Arg(0)
	}
	opts.repo = normalizeGitHubRepo(opts.repo)
	if opts.repo == "" {
		return fmt.Errorf("GitHub repo is required, use --repo owner/fixforge-client")
	}
	if strings.TrimSpace(opts.version) == "" {
		opts.version = "latest"
	}
	if opts.version == "latest" {
		version, err := latestGitHubRelease(opts.repo)
		if err != nil {
			return err
		}
		opts.version = version
	}
	if currentVersion != "" && currentVersion != "dev" && currentVersion == opts.version {
		fmt.Printf("fixforge-client is already at %s\n", currentVersion)
		return nil
	}
	return runUpdate(opts)
}

type updateOptions struct {
	version        string
	repo           string
	installPath    string
	installDir     string
	restartService bool
}

func runUpdate(opts updateOptions) error {
	targetPath, err := updateTargetPath(opts)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "fixforge-client-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	assetName, archivePath, err := downloadReleaseArchive(tmpDir, opts.repo, opts.version)
	if err != nil {
		return err
	}
	if err := verifyReleaseChecksum(opts.repo, opts.version, assetName, archivePath); err != nil {
		return err
	}
	binPath, err := extractClientBinary(archivePath, tmpDir)
	if err != nil {
		return err
	}

	serviceStopped := false
	if opts.restartService {
		if err := DoServiceStop(); err == nil {
			serviceStopped = true
		}
	}
	if err := installUpdatedBinary(binPath, targetPath); err != nil {
		return err
	}
	if serviceStopped {
		if err := DoServiceStart(); err != nil {
			return fmt.Errorf("updated binary but failed to restart service: %w", err)
		}
	}
	fmt.Printf("fixforge-client updated to %s at %s\n", opts.version, targetPath)
	return nil
}

func updateTargetPath(opts updateOptions) (string, error) {
	if opts.installPath != "" {
		return filepath.Abs(opts.installPath)
	}
	if opts.installDir != "" {
		path := filepath.Join(opts.installDir, platformBinaryName())
		return filepath.Abs(path)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	return filepath.Abs(exe)
}

func latestGitHubRelease(repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := getJSON(url, &payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.TagName) == "" {
		return "", fmt.Errorf("latest release for %s has empty tag_name", repo)
	}
	return payload.TagName, nil
}

func getJSON(url string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "fixforge-client")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{url: url, status: resp.StatusCode}
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func downloadReleaseArchive(tmpDir, repo, version string) (string, string, error) {
	var names []string
	base := fmt.Sprintf("%s_%s_%s_%s", clientBinaryName, version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		names = []string{base + ".zip", base + ".tar.gz"}
	} else {
		names = []string{base + ".tar.gz", base + ".zip"}
	}
	var lastErr error
	for _, name := range names {
		url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, version, name)
		dst := filepath.Join(tmpDir, name)
		if err := downloadFile(url, dst); err != nil {
			lastErr = err
			var statusErr *httpStatusError
			if errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound {
				continue
			}
			return "", "", err
		}
		return name, dst, nil
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", fmt.Errorf("no release archive found for %s %s/%s", version, runtime.GOOS, runtime.GOARCH)
}

func downloadFile(url, dst string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "fixforge-client")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{url: url, status: resp.StatusCode}
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func verifyReleaseChecksum(repo, version, assetName, archivePath string) error {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/checksums.txt", repo, version)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "fixforge-client")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	expected := checksumForAsset(string(data), assetName)
	if expected == "" {
		return nil
	}
	actual, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("checksum mismatch for %s", assetName)
	}
	return nil
}

func checksumForAsset(checksums, assetName string) string {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			return fields[0]
		}
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractClientBinary(archivePath, tmpDir string) (string, error) {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZipClientBinary(archivePath, tmpDir)
	}
	return extractTarGzClientBinary(archivePath, tmpDir)
}

func extractTarGzClientBinary(archivePath, tmpDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.FileInfo().IsDir() || filepath.Base(hdr.Name) != platformBinaryName() {
			continue
		}
		dst := filepath.Join(tmpDir, platformBinaryName())
		if err := writeExtractedBinary(dst, tr); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("%s not found in archive", platformBinaryName())
}

func extractZipClientBinary(archivePath, tmpDir string) (string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, file := range zr.File {
		if file.FileInfo().IsDir() || filepath.Base(file.Name) != platformBinaryName() {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return "", err
		}
		dst := filepath.Join(tmpDir, platformBinaryName())
		err = writeExtractedBinary(dst, rc)
		_ = rc.Close()
		if err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("%s not found in archive", platformBinaryName())
}

func writeExtractedBinary(dst string, src io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

func installUpdatedBinary(src, target string) error {
	if runtime.GOOS == "windows" {
		exe, _ := os.Executable()
		if samePath(exe, target) {
			return fmt.Errorf("Windows cannot replace the running executable; downloaded binary is at %s", src)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmpTarget := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".new")
	if err := copyFile(src, tmpTarget, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(target)
	}
	if err := os.Rename(tmpTarget, target); err != nil {
		_ = os.Remove(tmpTarget)
		return err
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func platformBinaryName() string {
	if runtime.GOOS == "windows" {
		return clientBinaryName + ".exe"
	}
	return clientBinaryName
}

func normalizeGitHubRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimPrefix(repo, "http://github.com/")
	repo = strings.TrimPrefix(repo, "git@github.com:")
	repo = strings.TrimSuffix(repo, ".git")
	return strings.Trim(repo, "/")
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}
