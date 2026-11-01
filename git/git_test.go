package git

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	wantTrue := []string{
		"git://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git",
		"git://example.com/repo",
		"ssh://git@github.com/rpajarola/mirrortools.git",
		"ssh://user@example.com:2222/srv/git/repo",
		"git+ssh://user@example.com/srv/git/repo",
		"git@github.com:rpajarola/mirrortools.git",
		"git@github.com:rpajarola/mirrortools",
		"user@example.com:repos/project.git",
		"example.com:repos/project.git",
		"https://example.com/foo/bar.git",
		"http://example.com/foo/bar.git/",
		"https://github.com/rpajarola/mirrortools",
		"https://github.com/rpajarola/mirrortools/",
		"https://www.github.com/rpajarola/mirrortools",
		"https://gitlab.com/group/project",
		"https://gitlab.com/group/subgroup/project",
		"https://codeberg.org/owner/repo",
		"https://bitbucket.org/owner/repo",
		"https://gitea.com/owner/repo",
		"https://git.sr.ht/~owner/repo",
		"https://go.googlesource.com/go",
	}
	for _, s := range wantTrue {
		if !Match(s) {
			t.Errorf("Match(%q) = false, want true", s)
		}
	}

	wantFalse := []string{
		"https://example.com/",
		"https://example.com/pub/",
		"https://example.com/repo.git?foo=bar",
		"https://github.com/",
		"https://github.com/rpajarola",
		"https://github.com/settings/profile",
		"https://github.com/orgs/some-org",
		"https://github.com/rpajarola/mirrortools/releases",
		"https://github.com/rpajarola/mirrortools/archive/refs/tags/v0.1.0.tar.gz",
		"https://gitlab.com/group/project/-/releases",
		"https://gitlab.com/explore/projects",
		"https://go.googlesource.com/go/+/refs/heads/master",
		"https://archive.org/details/some-item",
		"https://groups.io/g/somegroup",
		"ftp://ftp.example.com/pub/",
		"rsync://rsync.example.com/mod/",
		"rsync.example.com::mod/dir",
		"user@host:/srv/data",
		"imap.example.com:993",
		"someone@example.com",
		"./local/dir",
	}
	for _, s := range wantFalse {
		if Match(s) {
			t.Errorf("Match(%q) = true, want false", s)
		}
	}
}

func TestLocalPath(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"https://github.com/rpajarola/mirrortools", "/dest/github.com/rpajarola/mirrortools"},
		{"https://github.com/rpajarola/mirrortools/", "/dest/github.com/rpajarola/mirrortools"},
		{"https://github.com/rpajarola/mirrortools.git", "/dest/github.com/rpajarola/mirrortools"},
		{"https://GITHUB.COM:443/rpajarola/mirrortools.git", "/dest/github.com/rpajarola/mirrortools"},
		{"http://example.com:80/a/b.git", "/dest/example.com/a/b"},
		{"http://example.com:8080/a/b.git", "/dest/example.com:8080/a/b"},
		{"git://example.com:9418/a/b.git", "/dest/example.com/a/b"},
		{"ssh://git@example.com:22/a/b.git", "/dest/example.com/a/b"},
		{"ssh://git@example.com:2222/a/b.git", "/dest/example.com:2222/a/b"},
		{"git@github.com:rpajarola/mirrortools.git", "/dest/github.com/rpajarola/mirrortools"},
		{"git@github.com:rpajarola/mirrortools", "/dest/github.com/rpajarola/mirrortools"},
		{"user@example.com:~/repos/proj.git", "/dest/example.com/repos/proj"},
		{"https://example.com/../../etc/passwd.git", "/dest/example.com/etc/passwd"},
	}
	for _, tc := range cases {
		got, err := localPath("/dest", tc.source)
		if err != nil {
			t.Errorf("localPath(%q): %v", tc.source, err)
			continue
		}
		if got != filepath.FromSlash(tc.want) {
			t.Errorf("localPath(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}

	bad := []string{
		"https://example.com/",
		"https://example.com/.git",
		"ftp://example.com/repo.git",
		"host::module",
		"just-a-word",
	}
	for _, s := range bad {
		if got, err := localPath("/dest", s); err == nil {
			t.Errorf("localPath(%q) = %q, expected error", s, got)
		}
	}
}

// initTestRepo creates a bare repository at root/sub/repo.git and a working
// clone that pushes an initial commit and tag to it.
func initTestRepo(t *testing.T, root string) (workDir, bareDir string) {
	t.Helper()
	bareDir = filepath.Join(root, "sub", "repo.git")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, "", "init", "--bare", "-q", "-b", "main", bareDir)

	workDir = filepath.Join(t.TempDir(), "work")
	gitCmd(t, "", "init", "-q", "-b", "main", workDir)
	gitCmd(t, workDir, "config", "user.email", "test@example.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "README.md")
	gitCmd(t, workDir, "commit", "-q", "-m", "initial")
	gitCmd(t, workDir, "tag", "v1.0")
	gitCmd(t, workDir, "branch", "topic")
	gitCmd(t, workDir, "remote", "add", "origin", bareDir)
	gitCmd(t, workDir, "push", "-q", "origin", "main", "topic", "v1.0")
	return workDir, bareDir
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newSmartGitServer(t *testing.T, projectRoot string) *httptest.Server {
	t.Helper()
	execPath := gitCmd(t, "", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not found at %s: %v", backend, err)
	}
	h := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	return httptest.NewServer(h)
}

func TestMirrorSmartHTTP(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	workDir, bareDir := initTestRepo(t, root)
	// Also symlink sub/repo -> sub/repo.git so URLs without .git work.
	if err := os.Symlink("repo.git", filepath.Join(root, "sub", "repo")); err != nil {
		t.Fatal(err)
	}

	srv := newSmartGitServer(t, root)
	defer srv.Close()

	ctx := context.Background()
	repoURLNoExt := srv.URL + "/sub/repo"
	repoURLWithExt := srv.URL + "/sub/repo.git"

	if !ProbeHTTP(ctx, repoURLNoExt) {
		t.Errorf("ProbeHTTP(%s) = false, want true", repoURLNoExt)
	}
	if !ProbeHTTP(ctx, repoURLWithExt) {
		t.Errorf("ProbeHTTP(%s) = false, want true", repoURLWithExt)
	}
	if ProbeHTTP(ctx, srv.URL+"/nonexistent") {
		t.Errorf("ProbeHTTP(nonexistent) = true, want false")
	}

	destDir := t.TempDir()
	// Clone using URL without .git.
	if err := Mirror(ctx, repoURLNoExt, destDir); err != nil {
		t.Fatalf("initial Mirror: %v", err)
	}

	host := strings.TrimPrefix(srv.URL, "http://")
	mirrorDir := filepath.Join(destDir, host, "sub", "repo")
	if !isBareRepo(mirrorDir) {
		t.Fatalf("expected bare repo at %s", mirrorDir)
	}
	wantMain := gitCmd(t, bareDir, "rev-parse", "refs/heads/main")
	gotMain := gitCmd(t, mirrorDir, "rev-parse", "refs/heads/main")
	if gotMain != wantMain {
		t.Errorf("refs/heads/main = %s, want %s", gotMain, wantMain)
	}
	gotTag := gitCmd(t, mirrorDir, "rev-parse", "refs/tags/v1.0")
	if gotTag != wantMain {
		t.Errorf("refs/tags/v1.0 = %s, want %s", gotTag, wantMain)
	}

	// Advance main, delete topic on remote, then re-run Mirror using the
	// .git URL: it should update the same local mirror directory and prune
	// the deleted branch.
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "commit", "-q", "-a", "-m", "second")
	gitCmd(t, workDir, "push", "-q", "origin", "main", ":topic")

	if err := Mirror(ctx, repoURLWithExt, destDir); err != nil {
		t.Fatalf("update Mirror: %v", err)
	}
	wantMain2 := gitCmd(t, bareDir, "rev-parse", "refs/heads/main")
	gotMain2 := gitCmd(t, mirrorDir, "rev-parse", "refs/heads/main")
	if gotMain2 != wantMain2 || gotMain2 == wantMain {
		t.Errorf("after update: refs/heads/main = %s, want %s", gotMain2, wantMain2)
	}
	if err := exec.Command("git", "-C", mirrorDir, "rev-parse", "--verify", "refs/heads/topic").Run(); err == nil {
		t.Error("deleted branch refs/heads/topic should have been pruned")
	}
}

func TestMirrorDumbHTTP(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	_, bareDir := initTestRepo(t, root)
	gitCmd(t, bareDir, "update-server-info")

	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	defer srv.Close()

	ctx := context.Background()
	if !ProbeHTTP(ctx, srv.URL+"/sub/repo.git") {
		t.Errorf("ProbeHTTP(dumb HTTP) = false, want true")
	}

	destDir := t.TempDir()
	if err := Mirror(ctx, srv.URL+"/sub/repo.git", destDir); err != nil {
		t.Fatalf("Mirror dumb HTTP: %v", err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	mirrorDir := filepath.Join(destDir, host, "sub", "repo")
	if !isBareRepo(mirrorDir) {
		t.Fatalf("expected bare repo at %s", mirrorDir)
	}
}
