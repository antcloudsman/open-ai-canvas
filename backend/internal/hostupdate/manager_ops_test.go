package hostupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingRunner struct {
	calls [][]string
	fail  func(args []string) error
}

func (r *recordingRunner) Run(_ context.Context, _ string, args, _ []string, stdout, stderr io.Writer) error {
	return r.record(args, nil, stdout, stderr)
}

func (r *recordingRunner) RunWithInput(_ context.Context, _ string, args, _ []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return r.record(args, stdin, stdout, stderr)
}

func (r *recordingRunner) record(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	copied := append([]string(nil), args...)
	r.calls = append(r.calls, copied)
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, "backup-fixture")
	}
	if r.fail != nil {
		if err := r.fail(copied); err != nil {
			if stderr != nil {
				_, _ = io.WriteString(stderr, err.Error())
			}
			return err
		}
	}
	return nil
}

func TestSetEnvValuePreservesOtherSettings(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".env")
	if err := os.WriteFile(path, []byte("# keep\nCANVAS_IMAGE_TAG=1.0.0\nPOSTGRES_DB=canvas\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := setEnvValue(path, "CANVAS_IMAGE_TAG", "1.2.2-preview.1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if !strings.Contains(value, "# keep\n") || !strings.Contains(value, "POSTGRES_DB=canvas\n") || !strings.Contains(value, "CANVAS_IMAGE_TAG=1.2.2-preview.1\n") {
		t.Fatalf("unexpected env contents: %q", value)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%o, want 640", stat.Mode().Perm())
	}
}

func TestVerifyZipBackupRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, content := range map[string]string{
		"metadata.json":    "{}",
		"database.dump":    "database",
		"backend-data.tar": "data",
	} {
		entry, createErr := archive.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := io.WriteString(entry, content); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	checksum := "sha256:" + hex.EncodeToString(hash[:])
	if err := verifyZipBackup(path, checksum); err != nil {
		t.Fatalf("valid backup rejected: %v", err)
	}
	if err := os.WriteFile(path, append(data, byte(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyZipBackup(path, checksum); err == nil {
		t.Fatal("corrupted backup was accepted")
	}
}

func TestCurrentVersionRejectsLatest(t *testing.T) {
	installDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(installDir, ".env"), []byte("CANVAS_IMAGE_TAG=latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: Config{InstallDir: installDir, EnvFile: ".env"}}
	if _, err := manager.currentVersion(); err == nil {
		t.Fatal("latest tag was accepted")
	}
}

func TestCreateBackupReadsBackendDataAsRoot(t *testing.T) {
	installDir := t.TempDir()
	backupDir := filepath.Join(installDir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".env"), []byte("POSTGRES_USER=canvas\nPOSTGRES_DB=canvas\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	manager := &Manager{
		config: Config{InstallDir: installDir, ComposeFile: "docker-compose.deploy.yml", EnvFile: ".env", BackupDir: backupDir},
		runner: runner,
	}
	if _, err := manager.createBackup("v1.2.2-preview.2"); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "run --rm --no-deps --pull never -T --user root --entrypoint tar backend -C /data -cf - .") {
			return
		}
	}
	t.Fatalf("backend data backup did not use root: %#v", runner.calls)
}

func TestCheckWritableDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := checkWritableDirectory(directory); err != nil {
		t.Fatalf("writable directory rejected: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write probe was not cleaned up: %v", entries)
	}
	if err := checkWritableDirectory(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing directory was accepted")
	}
}

func TestComposeProjectNameUsesInstallDirectory(t *testing.T) {
	cases := map[string]string{
		"/opt/yingce":           "yingce",
		"/srv/Open AI Canvas_2": "openaicanvas_2",
		"/":                     "open-ai-canvas",
		"/srv/!!!":              "open-ai-canvas",
	}
	for installDir, want := range cases {
		if got := composeProjectName(installDir); got != want {
			t.Errorf("composeProjectName(%q) = %q, want %q", installDir, got, want)
		}
	}
}

func TestWriteComposeEnvOverrideReplacesImageRefs(t *testing.T) {
	installDir := t.TempDir()
	stateDir := filepath.Join(installDir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(installDir, ".env")
	if err := os.WriteFile(envPath, []byte("CANVAS_BACKEND_IMAGE=old-backend\nCANVAS_WEB_IMAGE=old-web\nPOSTGRES_DB=canvas\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: Config{InstallDir: installDir, EnvFile: ".env", StateDir: stateDir}}
	path, err := manager.writeComposeEnvOverride(deploymentImages{backend: "new-backend", web: "new-web", agent: "new-agent"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if !strings.Contains(value, "CANVAS_BACKEND_IMAGE=new-backend\n") || !strings.Contains(value, "CANVAS_WEB_IMAGE=new-web\n") || !strings.Contains(value, "CANVAS_YINGCE_AGENT_IMAGE=new-agent\n") || !strings.Contains(value, "POSTGRES_DB=canvas\n") {
		t.Fatalf("unexpected override env: %q", value)
	}
}

// v1.5.8.x 的 Host Updater 只注入 backend/web 镜像，也不会生成 Agent Token。
// 部署 Compose 中 Agent 相关变量必须有回退值，否则旧更新器的预检会直接失败。
func TestDeployComposeAgentVariablesFallbackForLegacyUpdater(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docker-compose.deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	compose := string(data)
	for _, required := range []string{"${CANVAS_YINGCE_AGENT_IMAGE:?", "${YINGCE_AGENT_TOKEN:?"} {
		if strings.Contains(compose, required) {
			t.Errorf("docker-compose.deploy.yml must not hard-require %s…}", required)
		}
	}
	for _, fallback := range []string{"${CANVAS_YINGCE_AGENT_IMAGE:-", "${YINGCE_AGENT_TOKEN:-${CANVAS_UPDATER_TOKEN:?"} {
		if !strings.Contains(compose, fallback) {
			t.Errorf("docker-compose.deploy.yml is missing fallback %s…}", fallback)
		}
	}
}

func TestForkDeployRepositoryDefaultAndOverride(t *testing.T) {
	if DefaultRepository != "antcloudsman/open-ai-canvas" {
		t.Fatalf("DefaultRepository = %q", DefaultRepository)
	}
	dir := t.TempDir()
	unset, err := NewManager(Config{
		InstallDir: filepath.Join(dir, "unset"),
		StateDir:   filepath.Join(dir, "state-unset"),
		BackupDir:  filepath.Join(dir, "backups-unset"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if unset.config.Repository != DefaultRepository {
		t.Fatalf("empty repository = %q", unset.config.Repository)
	}
	blank, err := NewManager(Config{
		Repository: " \t ",
		InstallDir: filepath.Join(dir, "blank"),
		StateDir:   filepath.Join(dir, "state-blank"),
		BackupDir:  filepath.Join(dir, "backups-blank"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if blank.config.Repository != DefaultRepository {
		t.Fatalf("blank repository = %q", blank.config.Repository)
	}
	override, err := NewManager(Config{
		Repository: " example/override ",
		InstallDir: filepath.Join(dir, "override"),
		StateDir:   filepath.Join(dir, "state-override"),
		BackupDir:  filepath.Join(dir, "backups-override"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if override.config.Repository != "example/override" {
		t.Fatalf("override repository = %q", override.config.Repository)
	}

	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docker-compose.deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	compose := string(data)
	const agentFallback = "${CANVAS_YINGCE_AGENT_IMAGE:-ghcr.io/antcloudsman/open-ai-canvas-yingce-agent:${CANVAS_IMAGE_TAG:?请先配置 CANVAS_IMAGE_TAG}}"
	if !strings.Contains(compose, agentFallback) {
		t.Fatalf("compose agent fallback missing %s", agentFallback)
	}
	if strings.Contains(compose, "ddcat-ai") {
		t.Fatal("docker-compose.deploy.yml still defaults to ddcat-ai")
	}
}

func TestRestorePreviousDeploymentStopsAgentBeforeDataAndDatabase(t *testing.T) {
	runner := &recordingRunner{}
	manager := testRollbackManager(t, runner)
	backup := writeBackupZip(t, t.TempDir(), sampleDataTar(t))
	started, err := manager.restorePreviousDeployment("v1.7.3.4", backup)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("expected services to start after restore")
	}
	stop := callIndex(runner.calls, "stop web backend yingce-agent")
	data := callIndex(runner.calls, "--entrypoint sh backend")
	drop := callIndex(runner.calls, " dropdb ")
	restore := callIndex(runner.calls, " pg_restore ")
	up := callIndex(runner.calls, "up -d --remove-orphans")
	if stop < 0 || data < 0 || drop < 0 || restore < 0 || up < 0 || !(stop < data && data < drop && drop < restore && restore < up) {
		t.Fatalf("rollback command order = %#v", commandSummaries(runner.calls))
	}
	if callIndex(runner.calls, "start backend yingce-agent web") >= 0 {
		t.Fatal("agent was started before the restored stack")
	}
	joined := strings.Join(runner.calls[data], " ")
	for _, required := range []string{"--project-name", "--pull never", "-i", "-T", "--user root", "--entrypoint sh", `data="/data"`, ".hostupdate-restore-new", ".hostupdate-restore-old"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("data restore command missing %s: %s", required, joined)
		}
	}
	logs := operationLogText(manager)
	dataLog := strings.Index(logs, "恢复后端数据目录，使文件与备份一致")
	dbLog := strings.Index(logs, "恢复 PostgreSQL")
	startLog := strings.Index(logs, "数据目录与数据库已恢复，正在启动旧版本服务")
	if dataLog < 0 || dbLog < dataLog || startLog < dbLog {
		t.Fatalf("logs = %q", logs)
	}
}

func TestRunRollbackStopFailureDoesNotRestoreOrHealthCheck(t *testing.T) {
	runner := &recordingRunner{fail: func(args []string) error {
		if strings.Contains(strings.Join(args, " "), "stop web backend yingce-agent") {
			return errors.New("stop failed")
		}
		return nil
	}}
	manager := testRollbackManager(t, runner)
	manager.runRollback("v1.7.3.4", Backup{}, true)
	if manager.state.Operation.Phase != PhaseManualIntervention {
		t.Fatalf("phase = %s", manager.state.Operation.Phase)
	}
	if !manager.state.Operation.AutomaticRollback {
		t.Fatal("automatic rollback flag was cleared")
	}
	if !strings.Contains(manager.state.Operation.RollbackError, "stop failed") {
		t.Fatalf("rollback error = %q", manager.state.Operation.RollbackError)
	}
	if callIndex(runner.calls, "--entrypoint sh backend") >= 0 || callIndex(runner.calls, " dropdb ") >= 0 || callIndex(runner.calls, "up -d") >= 0 {
		t.Fatalf("restore continued after stop failure: %#v", commandSummaries(runner.calls))
	}
	if callIndex(runner.calls, "start backend yingce-agent web") < 0 {
		t.Fatal("failed drain did not try to start the original services")
	}
	if !strings.Contains(operationLogText(manager), "已跳过数据恢复并尝试拉起原服务") {
		t.Fatalf("logs = %q", operationLogText(manager))
	}
}

func TestRestorePreviousDeploymentKeepsAgentStoppedWhenDataRestoreFails(t *testing.T) {
	runner := &recordingRunner{fail: func(args []string) error {
		if strings.Contains(strings.Join(args, " "), "--entrypoint sh backend") {
			return errors.New("tar failed")
		}
		return nil
	}}
	manager := testRollbackManager(t, runner)
	backup := writeBackupZip(t, t.TempDir(), sampleDataTar(t))
	started, err := manager.restorePreviousDeployment("v1.7.3.4", backup)
	if err == nil || started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	if !strings.Contains(err.Error(), "恢复数据目录") || !strings.Contains(err.Error(), restoreStagingDirName) {
		t.Fatalf("error = %v", err)
	}
	if callIndex(runner.calls, " dropdb ") >= 0 || callIndex(runner.calls, "up -d") >= 0 || callIndex(runner.calls, "start backend") >= 0 {
		t.Fatalf("services or database changed after data restore failure: %#v", commandSummaries(runner.calls))
	}
	if !strings.Contains(operationLogText(manager), "数据目录恢复失败，Web、Backend 与 Agent 保持停止") {
		t.Fatalf("logs = %q", operationLogText(manager))
	}
}

func TestRestorePreviousDeploymentKeepsAgentStoppedWhenDatabaseRestoreFails(t *testing.T) {
	runner := &recordingRunner{fail: func(args []string) error {
		if strings.Contains(strings.Join(args, " "), " dropdb ") {
			return errors.New("dropdb failed")
		}
		return nil
	}}
	manager := testRollbackManager(t, runner)
	backup := writeBackupZip(t, t.TempDir(), sampleDataTar(t))
	started, err := manager.restorePreviousDeployment("v1.7.3.4", backup)
	if err == nil || started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	if callIndex(runner.calls, "--entrypoint sh backend") < 0 {
		t.Fatal("data restore did not run before the database")
	}
	if callIndex(runner.calls, "up -d") >= 0 || callIndex(runner.calls, "start backend") >= 0 {
		t.Fatalf("agent was started after database restore failure: %#v", commandSummaries(runner.calls))
	}
	if !strings.Contains(operationLogText(manager), "数据库恢复失败，Web、Backend 与 Agent 保持停止") {
		t.Fatalf("logs = %q", operationLogText(manager))
	}
}

func TestRestorePreviousDeploymentSkipsRestoreWhenComposeCannotBeRestored(t *testing.T) {
	runner := &recordingRunner{}
	manager := testRollbackManager(t, runner)
	if err := os.Mkdir(manager.previousComposePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	backup := writeBackupZip(t, t.TempDir(), sampleDataTar(t))
	started, err := manager.restorePreviousDeployment("v1.7.3.4", backup)
	if err == nil || started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	if callIndex(runner.calls, "--entrypoint sh backend") >= 0 || callIndex(runner.calls, " dropdb ") >= 0 || callIndex(runner.calls, "up -d") >= 0 {
		t.Fatalf("restore continued without the previous compose file: %#v", commandSummaries(runner.calls))
	}
	if !strings.Contains(operationLogText(manager), "部署配置未恢复，已跳过数据库与数据目录恢复，服务保持停止") {
		t.Fatalf("logs = %q", operationLogText(manager))
	}
}

func TestRestoreDataDirectoryRejectsUnsafeTarWithoutDocker(t *testing.T) {
	runner := &recordingRunner{}
	manager := testRollbackManager(t, runner)
	backup := writeBackupZip(t, t.TempDir(), buildTar(t, []tarFixture{{name: "../etc/passwd", body: "secret"}}))
	if err := manager.restoreDataDirectory(backup); err == nil {
		t.Fatal("traversal archive was accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("docker was invoked for an unsafe archive: %#v", commandSummaries(runner.calls))
	}
	backup.Checksum = "sha256:" + strings.Repeat("ab", 32)
	if err := manager.restoreDataDirectory(backup); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
}

func TestValidateBackendDataTar(t *testing.T) {
	if err := validateBackendDataTar(bytes.NewReader(sampleDataTar(t))); err != nil {
		t.Fatal(err)
	}
	rejected := [][]tarFixture{
		{{name: "../etc/passwd", body: "x"}},
		{{name: "/etc/passwd", body: "x"}},
		{{name: "./link", typeflag: tar.TypeSymlink, link: "/etc/passwd"}},
		{{name: "./nested/link", typeflag: tar.TypeSymlink, link: "../../outside"}},
		{{name: "./hard", typeflag: tar.TypeLink, link: "../outside"}},
		{{name: "./hard", typeflag: tar.TypeLink}},
		{{name: "./dev", typeflag: tar.TypeChar}},
		{},
	}
	for _, entries := range rejected {
		if err := validateBackendDataTar(bytes.NewReader(buildTar(t, entries))); err == nil {
			t.Fatalf("accepted unsafe archive %#v", entries)
		}
	}
}

func TestRestoreBackendDataScriptReplacesVolume(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"only-old.txt", "old file.txt", filepath.Join("nested", "old.txt"), ".hidden"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runRestoreScript(t, root, buildTar(t, []tarFixture{
		{name: "./", typeflag: tar.TypeDir},
		{name: "./restored.txt", body: "new"},
		{name: "./nested/new.txt", body: "new"},
		{name: "./.kept-hidden", body: "hidden"},
		{name: "./.hostupdate-restore-new", body: "kept-marker-name"},
		{name: "./link", typeflag: tar.TypeSymlink, link: "restored.txt"},
	}))
	assertFile(t, filepath.Join(root, "restored.txt"), "new")
	assertFile(t, filepath.Join(root, "nested", "new.txt"), "new")
	assertFile(t, filepath.Join(root, ".kept-hidden"), "hidden")
	assertFile(t, filepath.Join(root, ".hostupdate-restore-new"), "kept-marker-name")
	link, err := os.Readlink(filepath.Join(root, "link"))
	if err != nil || link != "restored.txt" {
		t.Fatalf("symlink = %q, %v", link, err)
	}
	for _, name := range []string{"only-old.txt", "old file.txt", ".hidden", filepath.Join("nested", "old.txt"), ".hostupdate-restore-old"} {
		if _, statErr := os.Stat(filepath.Join(root, name)); !os.IsNotExist(statErr) {
			t.Fatalf("%s still present: %v", name, statErr)
		}
	}
	marker, err := os.Stat(filepath.Join(root, ".hostupdate-restore-new"))
	if err != nil || marker.IsDir() {
		t.Fatalf("restored marker name = dir %v, err %v", marker, err)
	}

	runRestoreScript(t, root, buildTar(t, []tarFixture{{name: "./second.txt", body: "second"}}))
	assertFile(t, filepath.Join(root, "second.txt"), "second")
	if _, statErr := os.Stat(filepath.Join(root, "restored.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("previous restore file still present: %v", statErr)
	}
}

func TestRestoreBackendDataScriptFailureLeavesMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "only-old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", strings.ReplaceAll(restoreBackendDataScript(), "/data", root))
	command.Stdin = strings.NewReader("not-a-tar")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid tar was accepted: %s", output)
	}
	assertFile(t, filepath.Join(root, "only-old.txt"), "old")
	marker, statErr := os.Stat(filepath.Join(root, ".hostupdate-restore-new"))
	if statErr != nil || !marker.IsDir() {
		t.Fatalf("staging marker = %v, %v", marker, statErr)
	}
	if _, statErr = os.Stat(filepath.Join(root, ".hostupdate-restore-old")); !os.IsNotExist(statErr) {
		t.Fatalf("database-side files were already swapped: %v", statErr)
	}
}

func runRestoreScript(t *testing.T, root string, payload []byte) {
	t.Helper()
	command := exec.Command("sh", "-c", strings.ReplaceAll(restoreBackendDataScript(), "/data", root))
	command.Stdin = bytes.NewReader(payload)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("restore script failed: %v\n%s", err, output)
	}
}

func testRollbackManager(t *testing.T, runner commandRunner) *Manager {
	t.Helper()
	install := t.TempDir()
	state := filepath.Join(install, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, ".env"), []byte("POSTGRES_USER=canvas\nPOSTGRES_DB=canvas\nCANVAS_IMAGE_TAG=1.7.3.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "docker-compose.deploy.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Manager{
		config: Config{
			InstallDir:  install,
			ComposeFile: "docker-compose.deploy.yml",
			EnvFile:     ".env",
			StateDir:    state,
			BackupDir:   filepath.Join(install, "backups"),
			StepTimeout: time.Minute,
		},
		runner: runner,
	}
}

type tarFixture struct {
	name     string
	body     string
	typeflag byte
	link     string
}

func sampleDataTar(t *testing.T) []byte {
	t.Helper()
	return buildTar(t, []tarFixture{
		{name: "./", typeflag: tar.TypeDir},
		{name: "./resources/a.bin", body: "object-bytes"},
		{name: "./nested/link", typeflag: tar.TypeSymlink, link: "../resources/a.bin"},
		{name: "./resources/hard", typeflag: tar.TypeLink, link: "./resources/a.bin"},
	})
}

func buildTar(t *testing.T, entries []tarFixture) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		flag := entry.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: 0o644, Typeflag: flag, Linkname: entry.link}
		if flag == tar.TypeDir {
			header.Mode = 0o755
		}
		if flag == tar.TypeReg || flag == tar.TypeRegA {
			header.Size = int64(len(entry.body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeBackupZip(t *testing.T, directory string, dataTar []byte) Backup {
	t.Helper()
	path := filepath.Join(directory, "backup.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, data := range map[string][]byte{
		"metadata.json":    []byte(`{"id":"backup-test"}`),
		"database.dump":    []byte("database-bytes"),
		"backend-data.tar": dataTar,
	} {
		entry, createErr := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write(data); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return Backup{
		ID:        "backup-test",
		Path:      path,
		Checksum:  "sha256:" + hex.EncodeToString(sum[:]),
		Size:      int64(len(data)),
		CreatedAt: time.Now().UTC(),
		Version:   "v1.7.3.5",
	}
}

func callIndex(calls [][]string, fragment string) int {
	for index, call := range calls {
		if strings.Contains(strings.Join(call, " "), fragment) {
			return index
		}
	}
	return -1
}

func commandSummaries(calls [][]string) []string {
	summaries := make([]string, 0, len(calls))
	for _, call := range calls {
		summaries = append(summaries, rollbackCommandDisplay(call))
	}
	return summaries
}

func operationLogText(manager *Manager) string {
	messages := make([]string, 0, len(manager.state.Operation.Logs))
	for _, entry := range manager.state.Operation.Logs {
		messages = append(messages, entry.Message)
	}
	return strings.Join(messages, "\n")
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
