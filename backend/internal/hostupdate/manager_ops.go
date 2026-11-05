package hostupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
}

func (m *Manager) latestRelease(ctx context.Context) (*Release, error) {
	url := "https://api.github.com/repos/" + m.config.Repository + "/releases?per_page=30"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "open-ai-canvas-host-updater")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if m.config.GitHubToken != "" {
		request.Header.Set("Authorization", "Bearer "+m.config.GitHubToken)
	}
	response, err := m.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub Release：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, fmt.Errorf("GitHub Release 返回 HTTP %d", response.StatusCode)
	}
	var releases []githubRelease
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReleaseResponseBytes))
	if err := decoder.Decode(&releases); err != nil {
		return nil, fmt.Errorf("解析 GitHub Release：%w", err)
	}
	filtered := make([]githubRelease, 0, len(releases))
	for _, release := range releases {
		if !release.Draft && strings.HasPrefix(release.TagName, "v") {
			filtered = append(filtered, release)
		}
	}
	if len(filtered) == 0 {
		return nil, errors.New("GitHub 尚未发布可用 Release")
	}
	sort.SliceStable(filtered, func(i, j int) bool { return CompareVersions(filtered[i].TagName, filtered[j].TagName) > 0 })
	latest := filtered[0]
	return &Release{Version: latest.TagName, Name: latest.Name, Body: latest.Body, URL: latest.HTMLURL, PublishedAt: latest.PublishedAt, Prerelease: latest.Prerelease}, nil
}

func (m *Manager) currentVersion() (string, error) {
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(values["CANVAS_IMAGE_TAG"])
	if version == "" || version == "latest" {
		return "", errors.New("CANVAS_IMAGE_TAG 必须固定为发布版本，不能使用 latest")
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return version, nil
}

func (m *Manager) prepareTargetCompose(targetVersion string) (string, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", m.config.Repository, targetVersion, m.config.ComposeFile)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := m.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("下载目标 Compose：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载目标 Compose 返回 HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", errors.New("目标 Compose 文件为空")
	}
	path := filepath.Join(m.config.StateDir, "compose-"+strings.TrimPrefix(targetVersion, "v")+".next.yml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("保存目标 Compose：%w", err)
	}
	return path, nil
}

func (m *Manager) preflight(composePath, targetVersion string) error {
	if _, err := os.Stat(m.composePath()); err != nil {
		return fmt.Errorf("读取当前 Compose：%w", err)
	}
	if _, err := os.Stat(m.envPath()); err != nil {
		return fmt.Errorf("读取部署环境：%w", err)
	}
	if m.config.SelfUpdate {
		if strings.TrimSpace(m.config.BinaryPath) == "" {
			return errors.New("检查 Host Updater 安装目录：二进制路径为空")
		}
		if err := checkWritableDirectory(filepath.Dir(m.config.BinaryPath)); err != nil {
			return fmt.Errorf("检查 Host Updater 安装目录：%w", err)
		}
	}
	if err := checkBackupDiskSpace(m.config.BackupDir); err != nil {
		return err
	}
	if err := m.composeWithImages(composePath, targetVersion, immutableImageRefs(m.config.Repository, targetVersion), 2*time.Minute, nil, "config", "--quiet"); err != nil {
		return fmt.Errorf("目标 Compose 校验失败：%w", err)
	}
	current, err := m.currentVersion()
	if err != nil {
		return err
	}
	if err := m.checkHealthOnce(m.healthURL(), current); err != nil {
		return fmt.Errorf("当前运行版本与部署配置不一致或服务未就绪：%w", err)
	}
	return nil
}

func checkWritableDirectory(directory string) error {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return errors.New("Host Updater 二进制路径为空")
	}
	temporary, err := os.CreateTemp(directory, ".updater-write-test-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if closeErr := temporary.Close(); closeErr != nil {
		_ = os.Remove(name)
		return closeErr
	}
	if removeErr := os.Remove(name); removeErr != nil {
		return removeErr
	}
	return nil
}

func (m *Manager) prepareUpdaterBinary(targetVersion string) (string, error) {
	if !m.config.SelfUpdate {
		return "", nil
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return "", fmt.Errorf("Host Updater 自更新不支持 %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if strings.TrimSpace(m.config.BinaryPath) == "" {
		return "", errors.New("未配置 Host Updater 二进制路径")
	}
	baseURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/", m.config.Repository, targetVersion)
	asset := "open-ai-canvas-host-updater-linux-" + runtime.GOARCH
	checksums, err := m.downloadReleaseAsset(baseURL+"SHA256SUMS", 1<<20)
	if err != nil {
		return "", fmt.Errorf("下载 Host Updater 校验清单：%w", err)
	}
	expected := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			expected = fields[0]
			break
		}
	}
	if len(expected) != 64 {
		return "", fmt.Errorf("目标 Release 的 SHA256SUMS 缺少 %s", asset)
	}
	binary, err := m.downloadReleaseAsset(baseURL+asset, 128<<20)
	if err != nil {
		return "", fmt.Errorf("下载目标 Host Updater：%w", err)
	}
	hash := sha256.Sum256(binary)
	actual := hex.EncodeToString(hash[:])
	if actual != expected {
		return "", fmt.Errorf("Host Updater 校验失败：期望 %s，实际 %s", expected, actual)
	}
	path := filepath.Join(m.config.StateDir, asset+"-"+strings.TrimPrefix(targetVersion, "v")+".next")
	if err := os.WriteFile(path, binary, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func (m *Manager) downloadReleaseAsset(url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "open-ai-canvas-host-updater")
	response, err := m.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	reader := io.LimitReader(response.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("Release 资产超过允许大小")
	}
	return data, nil
}

func (m *Manager) restartSelf() {
	time.Sleep(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = m.runner.Run(ctx, "systemctl", []string{"restart", m.config.ServiceName}, nil, io.Discard, io.Discard)
}

type deploymentImages struct {
	backend string
	web     string
	agent   string
}

func immutableImageRefs(repository, version string) deploymentImages {
	owner := strings.SplitN(repository, "/", 2)[0]
	tag := strings.TrimPrefix(version, "v")
	return deploymentImages{
		backend: "ghcr.io/" + owner + "/open-ai-canvas-backend:" + tag,
		web:     "ghcr.io/" + owner + "/open-ai-canvas-web:" + tag,
		agent:   "ghcr.io/" + owner + "/open-ai-canvas-yingce-agent:" + tag,
	}
}

func (m *Manager) compose(composePath, imageTag string, timeout time.Duration, stdout io.Writer, arguments ...string) error {
	return m.composeWithImages(composePath, imageTag, deploymentImages{}, timeout, stdout, arguments...)
}

func (m *Manager) composeWithImages(composePath, imageTag string, images deploymentImages, timeout time.Duration, stdout io.Writer, arguments ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	envFile := m.envPath()
	if images.backend != "" {
		var err error
		envFile, err = m.writeComposeEnvOverride(images)
		if err != nil {
			return fmt.Errorf("准备目标 Compose 环境：%w", err)
		}
		defer os.Remove(envFile)
	}
	args := []string{"compose", "--project-name", composeProjectName(m.config.InstallDir), "--env-file", envFile, "-f", composePath}
	args = append(args, arguments...)
	var stderr bytes.Buffer
	if stdout == nil {
		stdout = io.Discard
	}
	environment := []string{"CANVAS_IMAGE_TAG=" + strings.TrimPrefix(imageTag, "v")}
	err := m.runner.Run(ctx, "docker", args, environment, stdout, &stderr)
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[len(message)-1000:]
		}
		if message != "" {
			return fmt.Errorf("docker compose %s：%s", strings.Join(arguments, " "), message)
		}
		return fmt.Errorf("docker compose %s：%w", strings.Join(arguments, " "), err)
	}
	return nil
}

func (m *Manager) composeWithInput(composePath, imageTag string, timeout time.Duration, stdin io.Reader, arguments ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"compose", "--project-name", composeProjectName(m.config.InstallDir), "--env-file", m.envPath(), "-f", composePath}
	args = append(args, arguments...)
	var stderr bytes.Buffer
	environment := []string{"CANVAS_IMAGE_TAG=" + strings.TrimPrefix(imageTag, "v")}
	err := runWithInput(ctx, m.runner, "docker", args, environment, stdin, io.Discard, &stderr)
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[len(message)-1000:]
		}
		display := rollbackCommandDisplay(arguments)
		if message != "" {
			return fmt.Errorf("docker compose %s：%s", display, message)
		}
		return fmt.Errorf("docker compose %s：%w", display, err)
	}
	return nil
}

func rollbackCommandDisplay(arguments []string) string {
	display := make([]string, len(arguments))
	for index, argument := range arguments {
		if strings.Contains(argument, restoreStagingDirName) {
			display[index] = "<restore-data-script>"
			continue
		}
		display[index] = argument
	}
	return strings.Join(display, " ")
}

func runWithInput(ctx context.Context, runner commandRunner, name string, args, environment []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if withInput, ok := runner.(stdinCommandRunner); ok {
		return withInput.RunWithInput(ctx, name, args, environment, stdin, stdout, stderr)
	}
	if stdin == nil {
		return runner.Run(ctx, name, args, environment, stdout, stderr)
	}
	return errors.New("当前命令执行器不支持标准输入")
}

func composeProjectName(installDir string) string {
	name := filepath.Base(filepath.Clean(installDir))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "open-ai-canvas"
	}
	var builder strings.Builder
	for _, char := range strings.ToLower(name) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		}
	}
	if result := strings.Trim(builder.String(), "-_ "); result != "" {
		return result
	}
	return "open-ai-canvas"
}

func (m *Manager) writeComposeEnvOverride(images deploymentImages) (string, error) {
	data, err := os.ReadFile(m.envPath())
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.config.StateDir, ".compose-env-override-"+randomID())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	cleanup := func() { _ = os.Remove(path) }
	if err := setEnvValue(path, "CANVAS_BACKEND_IMAGE", images.backend); err != nil {
		cleanup()
		return "", err
	}
	if err := setEnvValue(path, "CANVAS_WEB_IMAGE", images.web); err != nil {
		cleanup()
		return "", err
	}
	if err := setEnvValue(path, "CANVAS_YINGCE_AGENT_IMAGE", images.agent); err != nil {
		cleanup()
		return "", err
	}
	return path, nil
}

func (m *Manager) verifyImages(targetVersion string) (deploymentImages, error) {
	images := immutableImageRefs(m.config.Repository, targetVersion)
	refs := []string{images.backend, images.web, images.agent}
	digests := make([]string, 0, len(refs))
	for _, image := range refs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		var output bytes.Buffer
		var stderr bytes.Buffer
		err := m.runner.Run(ctx, "docker", []string{"image", "inspect", image, "--format", "{{json .RepoDigests}}"}, nil, &output, &stderr)
		cancel()
		if err != nil {
			return deploymentImages{}, fmt.Errorf("校验目标镜像摘要失败：%s", strings.TrimSpace(stderr.String()))
		}
		var repositoryDigests []string
		if err := json.Unmarshal(output.Bytes(), &repositoryDigests); err != nil {
			return deploymentImages{}, fmt.Errorf("解析目标镜像摘要失败：%w", err)
		}
		repository := strings.Split(image, ":")[0]
		var digest string
		for _, candidate := range repositoryDigests {
			if strings.HasPrefix(candidate, repository+"@sha256:") {
				digest = candidate
				break
			}
		}
		if digest == "" {
			return deploymentImages{}, fmt.Errorf("目标镜像 %s 未包含仓库摘要", image)
		}
		digests = append(digests, digest)
	}
	return deploymentImages{backend: digests[0], web: digests[1], agent: digests[2]}, nil
}

func setDeploymentImages(path, version string, images deploymentImages) error {
	if err := setEnvValue(path, "CANVAS_IMAGE_TAG", strings.TrimPrefix(version, "v")); err != nil {
		return err
	}
	if err := setEnvValue(path, "CANVAS_BACKEND_IMAGE", images.backend); err != nil {
		return err
	}
	if err := setEnvValue(path, "CANVAS_WEB_IMAGE", images.web); err != nil {
		return err
	}
	if err := setEnvValue(path, "CANVAS_YINGCE_AGENT_IMAGE", images.agent); err != nil {
		return err
	}
	return ensureAgentToken(path)
}

func ensureAgentToken(path string) error {
	values, err := readEnvFile(path)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(values["YINGCE_AGENT_TOKEN"])) >= 32 {
		return nil
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("生成 Agent 服务凭证：%w", err)
	}
	return setEnvValue(path, "YINGCE_AGENT_TOKEN", hex.EncodeToString(token))
}

func (m *Manager) createBackup(version string) (Backup, error) {
	now := time.Now().UTC()
	id := "backup-" + now.Format("20060102-150405")
	path := filepath.Join(m.config.BackupDir, id+".zip")
	temporary := path + ".partial"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Backup{}, fmt.Errorf("创建备份文件：%w", err)
	}
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	archive := zip.NewWriter(io.MultiWriter(file, hasher))
	metadata, _ := json.MarshalIndent(map[string]any{"id": id, "version": version, "createdAt": now, "format": 1}, "", "  ")
	if err := writeZipBytes(archive, "metadata.json", metadata); err != nil {
		return Backup{}, err
	}
	databaseEntry, err := archive.CreateHeader(&zip.FileHeader{Name: "database.dump", Method: zip.Store})
	if err != nil {
		return Backup{}, err
	}
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return Backup{}, err
	}
	postgresUser := firstNonEmpty(values["POSTGRES_USER"], "open_ai_canvas")
	postgresDB := firstNonEmpty(values["POSTGRES_DB"], "open_ai_canvas")
	if err := m.compose(m.composePath(), version, m.config.StepTimeout, databaseEntry, "exec", "-T", "postgres", "pg_dump", "-U", postgresUser, "-d", postgresDB, "-Fc"); err != nil {
		return Backup{}, fmt.Errorf("备份 PostgreSQL：%w", err)
	}
	dataEntry, err := archive.CreateHeader(&zip.FileHeader{Name: "backend-data.tar", Method: zip.Store})
	if err != nil {
		return Backup{}, err
	}
	// Backend 已停止；复用旧镜像与原数据卷，只执行 tar，不启动业务或依赖服务。
	if err := m.compose(m.composePath(), version, m.config.StepTimeout, dataEntry, "run", "--rm", "--no-deps", "--pull", "never", "-T", "--user", "root", "--entrypoint", "tar", "backend", "-C", "/data", "-cf", "-", "."); err != nil {
		return Backup{}, fmt.Errorf("备份数据目录：%w", err)
	}
	if err := archive.Close(); err != nil {
		return Backup{}, fmt.Errorf("完成 ZIP 备份：%w", err)
	}
	if err := file.Sync(); err != nil {
		return Backup{}, fmt.Errorf("同步 ZIP 备份：%w", err)
	}
	if err := file.Close(); err != nil {
		return Backup{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return Backup{}, fmt.Errorf("提交 ZIP 备份：%w", err)
	}
	removeTemporary = false
	stat, err := os.Stat(path)
	if err != nil {
		return Backup{}, err
	}
	checksum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if err := verifyZipBackup(path, checksum); err != nil {
		return Backup{}, err
	}
	return Backup{ID: id, Path: path, Checksum: checksum, Size: stat.Size(), CreatedAt: now, Version: version}, nil
}

func writeZipBytes(writer *zip.Writer, name string, data []byte) error {
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return err
	}
	_, err = entry.Write(data)
	return err
}

func verifyZipBackup(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	actual := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if actual != expected {
		return fmt.Errorf("ZIP 备份校验失败：期望 %s，实际 %s", expected, actual)
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("ZIP 备份无法打开：%w", err)
	}
	defer archive.Close()
	required := map[string]bool{"metadata.json": false, "database.dump": false, "backend-data.tar": false}
	for _, entry := range archive.File {
		if _, ok := required[entry.Name]; ok && entry.UncompressedSize64 > 0 {
			required[entry.Name] = true
		}
	}
	for name, present := range required {
		if !present {
			return fmt.Errorf("ZIP 备份缺少有效文件 %s", name)
		}
	}
	return nil
}

func (m *Manager) restoreDatabase(backup Backup) error {
	if err := verifyZipBackup(backup.Path, backup.Checksum); err != nil {
		return fmt.Errorf("拒绝恢复未通过校验的备份：%w", err)
	}
	archive, err := zip.OpenReader(backup.Path)
	if err != nil {
		return err
	}
	defer archive.Close()
	var dump *zip.File
	for _, entry := range archive.File {
		if entry.Name == "database.dump" {
			dump = entry
			break
		}
	}
	if dump == nil {
		return errors.New("备份中不存在 database.dump")
	}
	reader, err := dump.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return err
	}
	postgresUser := firstNonEmpty(values["POSTGRES_USER"], "open_ai_canvas")
	postgresDB := firstNonEmpty(values["POSTGRES_DB"], "open_ai_canvas")
	if postgresDB == "postgres" || strings.HasPrefix(postgresDB, "template") {
		return fmt.Errorf("拒绝覆盖 PostgreSQL 系统数据库 %q", postgresDB)
	}
	if err := m.compose(m.composePath(), backup.Version, 5*time.Minute, nil, "exec", "-T", "postgres", "dropdb", "-U", postgresUser, "--if-exists", "--force", postgresDB); err != nil {
		return fmt.Errorf("删除待恢复数据库：%w", err)
	}
	if err := m.compose(m.composePath(), backup.Version, 5*time.Minute, nil, "exec", "-T", "postgres", "createdb", "-U", postgresUser, "-O", postgresUser, postgresDB); err != nil {
		return fmt.Errorf("重建待恢复数据库：%w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.config.StepTimeout)
	defer cancel()
	args := []string{"compose", "--env-file", m.envPath(), "-f", m.composePath(), "exec", "-T", "postgres", "pg_restore", "-U", postgresUser, "-d", postgresDB, "--no-owner", "--no-privileges"}
	var stderr bytes.Buffer
	if err := runWithInput(ctx, m.runner, "docker", args, []string{"CANVAS_IMAGE_TAG=" + strings.TrimPrefix(backup.Version, "v")}, reader, io.Discard, &stderr); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("恢复 PostgreSQL：%s", message)
	}
	return nil
}

const (
	restoreStagingDirName  = ".hostupdate-restore-new"
	restorePreviousDirName = ".hostupdate-restore-old"
)

// restoreBackendDataScript 用标准输入里的 tar 替换 /data。
// 替换未完成时会留下 .hostupdate-restore-new 或 .hostupdate-restore-old，用来识别半恢复。
// 这两个名字以及 .hostupdate-restore-* 只给回退使用；开始时会清掉上次留下的标记。
func restoreBackendDataScript() string {
	return `set -eu
data="/data"
case "$data" in
  "/data") ;;
  *) echo "拒绝异常的数据目录" >&2; exit 1 ;;
esac
for entry in "$data"/.hostupdate-restore-*; do
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  rm -rf "$entry"
done
staging="$data/.hostupdate-restore-new"
mkdir -p "$staging"
tar -C "$staging" -xf -
previous="$data/.hostupdate-restore-old"
mkdir -p "$previous"
for entry in "$data"/* "$data"/.[!.]* "$data"/..?*; do
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  name=${entry##*/}
  case "$name" in
    .hostupdate-restore-*) continue ;;
  esac
  mv -- "$entry" "$previous"/
done
incoming="$previous/.hostupdate-restore-incoming"
mv -- "$staging" "$incoming"
for entry in "$incoming"/* "$incoming"/.[!.]* "$incoming"/..?*; do
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  mv -- "$entry" "$data"/
done
sync
rm -rf "$previous"
`
}

func (m *Manager) restoreDataDirectory(backup Backup) error {
	if err := verifyZipBackup(backup.Path, backup.Checksum); err != nil {
		return fmt.Errorf("拒绝恢复未通过校验的备份：%w", err)
	}
	entry, err := openZipEntry(backup.Path, "backend-data.tar")
	if err != nil {
		return err
	}
	validateErr := validateBackendDataTar(entry)
	closeErr := entry.Close()
	if validateErr != nil {
		return fmt.Errorf("校验数据目录备份：%w", validateErr)
	}
	if closeErr != nil {
		return closeErr
	}
	entry, err = openZipEntry(backup.Path, "backend-data.tar")
	if err != nil {
		return err
	}
	defer entry.Close()
	// 与备份相同：停服后的一次性 root 容器，只跑 tar，不启动业务入口。项目名必须和备份时一致，否则会写到另一个数据卷。
	if err := m.composeWithInput(m.composePath(), backup.Version, m.config.StepTimeout, entry, "run", "--rm", "--no-deps", "--pull", "never", "-i", "-T", "--user", "root", "--entrypoint", "sh", "backend", "-c", restoreBackendDataScript()); err != nil {
		return fmt.Errorf("恢复数据目录：%w；数据卷中若仍有 %s 或 %s，说明替换未完成，服务保持停止", err, restoreStagingDirName, restorePreviousDirName)
	}
	return nil
}

func openZipEntry(zipPath, name string) (io.ReadCloser, error) {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	for _, entry := range archive.File {
		if entry.Name != name {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			archive.Close()
			return nil, err
		}
		return &zipEntryReader{Reader: reader, closers: []io.Closer{reader, archive}}, nil
	}
	archive.Close()
	return nil, fmt.Errorf("备份中不存在 %s", name)
}

type zipEntryReader struct {
	io.Reader
	closers []io.Closer
}

func (r *zipEntryReader) Close() error {
	var err error
	for _, closer := range r.closers {
		err = errors.Join(err, closer.Close())
	}
	return err
}

func validateBackendDataTar(reader io.Reader) error {
	archive := tar.NewReader(reader)
	count := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取数据目录备份：%w", err)
		}
		if err := validateBackendDataTarHeader(header); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, archive); err != nil {
			return fmt.Errorf("读取数据目录备份：%w", err)
		}
		switch header.Typeflag {
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
		default:
			count++
		}
	}
	if count == 0 {
		return errors.New("数据目录备份为空")
	}
	return nil
}

func validateBackendDataTarHeader(header *tar.Header) error {
	switch header.Typeflag {
	case tar.TypeXHeader, tar.TypeXGlobalHeader:
		return nil
	case tar.TypeReg, tar.TypeRegA, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
	default:
		return fmt.Errorf("数据目录备份包含不支持的文件类型 %q", header.Name)
	}
	relative, err := archiveRelative(header.Name)
	if err != nil {
		return fmt.Errorf("数据目录备份包含越界路径 %q", header.Name)
	}
	if relative == "" && header.Typeflag != tar.TypeDir && !strings.HasSuffix(header.Name, "/") {
		return fmt.Errorf("数据目录备份包含空路径 %q", header.Name)
	}
	switch header.Typeflag {
	case tar.TypeSymlink:
		if err := symlinkWithinArchive(header.Name, header.Linkname); err != nil {
			return fmt.Errorf("数据目录备份包含越界链接 %q -> %q", header.Name, header.Linkname)
		}
	case tar.TypeLink:
		if header.Linkname == "" {
			return fmt.Errorf("数据目录备份包含空硬链接 %q", header.Name)
		}
		if _, err := archiveRelative(header.Linkname); err != nil {
			return fmt.Errorf("数据目录备份包含越界硬链接 %q -> %q", header.Name, header.Linkname)
		}
	}
	return nil
}

func symlinkWithinArchive(name, target string) error {
	if target == "" || strings.HasPrefix(target, "/") || strings.HasPrefix(target, `\`) {
		return errors.New("无效链接")
	}
	directory, err := archiveRelative(path.Dir(name))
	if err != nil {
		return err
	}
	combined := target
	if directory != "" {
		combined = directory + "/" + target
	}
	if _, err := archiveRelative(combined); err != nil {
		return err
	}
	return nil
}

func archiveRelative(name string) (string, error) {
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return "", errors.New("绝对路径")
	}
	var parts []string
	for _, part := range strings.Split(name, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) == 0 {
				return "", errors.New("越界路径")
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/"), nil
}

func (m *Manager) verifyHealthy(targetVersion string) error {
	healthURL := m.healthURL()
	deadline := time.Now().Add(10 * time.Minute)
	stableSince := time.Time{}
	for time.Now().Before(deadline) {
		if err := m.checkHealthOnce(healthURL, targetVersion); err != nil {
			stableSince = time.Time{}
		} else if stableSince.IsZero() {
			stableSince = time.Now()
		} else if time.Since(stableSince) >= m.config.StableWindow {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("目标版本 %s 未在健康检查窗口内稳定就绪", targetVersion)
}

func (m *Manager) healthURL() string {
	if configured := strings.TrimSpace(m.config.HealthURL); configured != "" {
		return configured
	}
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return "http://127.0.0.1:3000/api/health/ready"
	}
	return "http://127.0.0.1:" + firstNonEmpty(values["CANVAS_HTTP_PORT"], "3000") + "/api/health/ready"
}

func (m *Manager) checkHealthOnce(healthURL, targetVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	response, err := m.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("健康接口返回 HTTP %d", response.StatusCode)
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Build struct {
				Version string `json:"version"`
			} `json:"build"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(&payload); err != nil {
		return err
	}
	if payload.Code != 0 {
		return errors.New("健康接口业务状态异常")
	}
	if IsReleaseVersion(targetVersion) && payload.Data.Build.Version != "" && CompareVersions(payload.Data.Build.Version, targetVersion) != 0 {
		return fmt.Errorf("运行版本仍为 %s，期望 %s", payload.Data.Build.Version, targetVersion)
	}
	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"")
		}
	}
	return values, nil
}

func setEnvValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	found := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+"=") {
			lines[index] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	stat, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".env-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(stat.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func replaceFile(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if mode == 0 {
		if stat, statErr := os.Stat(target); statErr == nil {
			mode = stat.Mode().Perm()
		} else {
			mode = 0o600
		}
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".compose-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, target)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
