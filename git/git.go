// Package git mirrors a remote Git repository into a local directory as a
// working-tree clone (`git clone`), or updates an existing clone in place
// (`git fetch --prune --tags` and fast-forwarding the checked-out branch).
// An existing bare clone at the target path is converted to a non-bare
// working-tree clone automatically without re-downloading objects.
//
// It registers itself as the "git" method with mirrortools' mirror package,
// so it's normally driven through the mirror CLI:
//
//	mirror git https://github.com/user/repo ./mirror
//	mirror https://github.com/user/repo ./mirror
//	mirror git@github.com:user/repo.git ./mirror
//
// The repository lands at <destDir>/<host>/<path> (with any trailing
// ".git" stripped from <path>, so https://host/user/repo and
// https://host/user/repo.git and git@host:user/repo.git all update the same
// local mirror).
package git

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name: "git",
		Source: "a git repository URL: https://, http://, git://, ssh://, " +
			"or [user@]host:path",
		Describe: "mirror a git repository (`git clone` / `git fetch` + fast-forward)",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			return func(ctx context.Context, source, destDir string) error {
				return Mirror(ctx, source, destDir)
			}
		},
		Match: Match,
	})
}

// tmpSuffix marks an initial clone in progress: git clone --mirror writes to
// <repoDir>.gitmirror-tmp first and renames it into place once complete, so
// an interrupted clone never leaves a half-initialized bare repository at
// the final path.
const tmpSuffix = ".gitmirror-tmp"

const probeTimeout = 30 * time.Second

// reservedForgeRoots are top-level path segments on GitHub, GitLab, Codeberg,
// Bitbucket and Gitea that are site pages rather than user/organization names.
var reservedForgeRoots = map[string]bool{
	"-": true, "about": true, "abuse_reports": true, "account": true,
	"admin": true, "apps": true, "blog": true, "codespaces": true,
	"collections": true, "contact": true, "copilot": true,
	"customer-stories": true, "dashboard": true, "docs": true,
	"enterprise": true, "events": true, "explore": true, "features": true,
	" groups": true, "groups": true, "help": true, "issues": true,
	"join": true, "login": true, "marketplace": true, "new": true,
	"notifications": true, "orgs": true, "organizations": true,
	"pricing": true, "pulls": true, "readme": true, "search": true,
	"security": true, "session": true, "settings": true, "signin": true,
	"signup": true, "site": true, "snippets": true, "sponsors": true,
	"status": true, "support": true, "team": true, "terms": true,
	"topics": true, "trending": true, "user": true, "users": true,
}

// Match reports whether source is recognizable from its syntax alone as a
// Git repository URL:
//
//   - git://, ssh://, or git+ssh:// URLs;
//   - SCP-style git@host:path or [user@]host:path.git sources;
//   - http:// or https:// URLs ending in ".git" (with optional trailing "/");
//   - http:// or https:// repository root URLs on major git forges
//     (github.com, gitlab.com, codeberg.org, bitbucket.org, gitea.com,
//     git.sr.ht, *.googlesource.com).
//
// Arbitrary http(s):// URLs that serve a Git repository without a ".git"
// suffix on other hosts are detected at runtime via ProbeHTTP.
func Match(source string) bool {
	if strings.HasPrefix(source, "git://") ||
		strings.HasPrefix(source, "ssh://") ||
		strings.HasPrefix(source, "git+ssh://") {
		_, _, err := parseSource(source)
		return err == nil
	}

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		u, err := url.Parse(source)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		if _, _, err := parseSource(source); err != nil {
			return false
		}
		p := strings.TrimSuffix(u.Path, "/")
		if strings.HasSuffix(p, ".git") && p != "/.git" && p != ".git" {
			return true
		}
		return isKnownForgeRepo(strings.ToLower(u.Hostname()), p)
	}

	// SCP-style [user@]host:path: only claim it automatically when the
	// user is "git" (git@host:path) or the path ends in ".git", so plain
	// host:port (IMAP) or user@host:path (rsync) isn't hijacked.
	if !strings.Contains(source, "://") && !strings.Contains(source, "::") {
		p := strings.TrimSuffix(source, "/")
		if strings.HasPrefix(p, "git@") || strings.HasSuffix(p, ".git") {
			_, _, err := parseSource(source)
			return err == nil
		}
	}
	return false
}

func isKnownForgeRepo(host, p string) bool {
	host = strings.TrimPrefix(host, "www.")
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return false
	}
	switch {
	case host == "github.com" || host == "codeberg.org" || host == "bitbucket.org" || host == "gitea.com":
		return len(segs) == 2 && !reservedForgeRoots[strings.ToLower(segs[0])]
	case host == "git.sr.ht":
		return len(segs) == 2 && strings.HasPrefix(segs[0], "~") && len(segs[0]) > 1
	case host == "gitlab.com":
		if len(segs) < 2 || reservedForgeRoots[strings.ToLower(segs[0])] {
			return false
		}
		for _, s := range segs {
			if s == "-" {
				return false
			}
		}
		return true
	case strings.HasSuffix(host, ".googlesource.com"):
		for _, s := range segs {
			if strings.HasPrefix(s, "+") {
				return false
			}
		}
		return true
	}
	return false
}

var dumbInfoRefsLineRe = regexp.MustCompile(`^[0-9a-f]{40,64}\trefs/\S+$`)

// ProbeHTTP reports whether rawURL (an http:// or https:// URL) serves a Git
// repository over Smart HTTP or Dumb HTTP by probing
// <url>/info/refs?service=git-upload-pack.
func ProbeHTTP(ctx context.Context, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	client := &http.Client{Timeout: probeTimeout}
	base := strings.TrimRight(u.String(), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "git/2.0 (mirrortools)")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "application/x-git-upload-pack-advertisement" {
		return true
	}
	if mt == "text/html" || mt == "application/xhtml+xml" {
		return false
	}

	// Dumb HTTP: /info/refs is a tab-separated list of "<sha>\trefs/...",
	// and /HEAD begins with "ref: refs/" (or a bare object ID).
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line != "" && !dumbInfoRefsLineRe.MatchString(line) {
			return false
		}
	}

	headReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/HEAD", nil)
	if err != nil {
		return false
	}
	headReq.Header.Set("User-Agent", "git/2.0 (mirrortools)")
	headResp, err := client.Do(headReq)
	if err != nil {
		return false
	}
	defer headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		return false
	}
	headBody, err := io.ReadAll(io.LimitReader(headResp.Body, 256))
	if err != nil {
		return false
	}
	return bytes.HasPrefix(headBody, []byte("ref: refs/"))
}

// Mirror clones source as a working-tree repository into
// <destDir>/<host>/<path> if not already present, or updates an existing
// clone in place. If <destDir>/<host>/<path> is a bare repository (for
// example from an earlier `git clone --mirror`), it is converted in place to
// a non-bare working-tree clone without re-downloading objects before
// updating.
func Mirror(ctx context.Context, source, destDir string) error {
	repoDir, err := localPath(destDir, source)
	if err != nil {
		return err
	}

	if isGitDir(repoDir) && !isGitDir(filepath.Join(repoDir, ".git")) {
		log.Printf("git: converting bare repository %s to working-tree clone", repoDir)
		if err := convertBareToWorktree(ctx, repoDir); err != nil {
			return err
		}
	}

	if isGitDir(filepath.Join(repoDir, ".git")) {
		log.Printf("git: updating %s in %s", source, repoDir)
		if err := updateWorktree(ctx, repoDir, source); err != nil {
			return err
		}
		log.Printf("git: done: %s", repoDir)
		return nil
	}

	log.Printf("git: cloning %s into %s", source, repoDir)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	tmpDir := repoDir + tmpSuffix
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	if err := runGit(ctx, "clone", "--", source, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	// If repoDir already exists as an empty directory, remove it so Rename
	// succeeds across platforms.
	_ = os.Remove(repoDir)
	if err := os.Rename(tmpDir, repoDir); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("git: %w", err)
	}
	log.Printf("git: done: %s", repoDir)
	return nil
}

// convertBareToWorktree moves a bare repository at repoDir into repoDir/.git,
// reconfigures it as a standard non-bare clone, turns non-HEAD local branches
// from `git clone --mirror` into remote-tracking refs under
// refs/remotes/origin/*, and checks out HEAD into the working tree.
func convertBareToWorktree(ctx context.Context, repoDir string) error {
	tmpDir := repoDir + tmpSuffix
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	if err := os.Rename(repoDir, tmpDir); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		_ = os.Rename(tmpDir, repoDir)
		return fmt.Errorf("git: %w", err)
	}
	if err := os.Rename(tmpDir, filepath.Join(repoDir, ".git")); err != nil {
		_ = os.Remove(repoDir)
		_ = os.Rename(tmpDir, repoDir)
		return fmt.Errorf("git: %w", err)
	}

	if err := runGit(ctx, "-C", repoDir, "config", "--bool", "core.bare", "false"); err != nil {
		return err
	}
	_ = exec.CommandContext(ctx, "git", "-C", repoDir, "config", "--unset-all", "remote.origin.mirror").Run()
	if err := runGit(ctx, "-C", repoDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return err
	}

	headRef, _ := gitOutput(ctx, "-C", repoDir, "symbolic-ref", "--quiet", "HEAD")
	headBranch := strings.TrimPrefix(headRef, "refs/heads/")

	refsOut, err := gitOutput(ctx, "-C", repoDir, "for-each-ref", "--format=%(refname:lstrip=2) %(objectname)", "refs/heads")
	if err == nil && refsOut != "" {
		for _, line := range strings.Split(refsOut, "\n") {
			branch, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || branch == "" || sha == "" {
				continue
			}
			_ = runGit(ctx, "-C", repoDir, "update-ref", "refs/remotes/origin/"+branch, sha)
			if branch != headBranch {
				_ = runGit(ctx, "-C", repoDir, "branch", "-D", branch)
			}
		}
	}

	if hasCommit(ctx, repoDir, "HEAD") {
		if err := runGit(ctx, "-C", repoDir, "checkout", "-f", "HEAD"); err != nil {
			return err
		}
	}
	return nil
}

// updateWorktree fetches all remote branches and tags (pruning deleted
// remote branches) and fast-forwards the checked-out branch if its remote
// counterpart exists.
func updateWorktree(ctx context.Context, repoDir, source string) error {
	if err := runGit(ctx, "-C", repoDir, "remote", "set-url", "origin", "--", source); err != nil {
		return err
	}
	if err := runGit(ctx, "-C", repoDir, "fetch", "--prune", "--tags", "origin"); err != nil {
		return err
	}

	headRef, err := gitOutput(ctx, "-C", repoDir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return nil // detached HEAD; fetch is enough
	}
	branch := strings.TrimPrefix(headRef, "refs/heads/")
	remoteRef := "refs/remotes/origin/" + branch
	if !hasCommit(ctx, repoDir, remoteRef) {
		return nil
	}

	if !hasCommit(ctx, repoDir, "HEAD") {
		return runGit(ctx, "-C", repoDir, "checkout", "-B", branch, "--track", "origin/"+branch)
	}
	_ = runGit(ctx, "-C", repoDir, "branch", "--set-upstream-to=origin/"+branch, branch)
	return runGit(ctx, "-C", repoDir, "merge", "--ff-only", remoteRef)
}

func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

func gitOutput(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func hasCommit(ctx context.Context, repoDir, rev string) bool {
	return exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "--verify", "-q", rev+"^{commit}").Run() == nil
}

// isGitDir reports whether dir looks like a Git directory (either a bare
// repository root or a working-tree clone's .git subdirectory): it has a
// regular HEAD file and objects/ and refs/ directories.
func isGitDir(dir string) bool {
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objs, err := os.Stat(filepath.Join(dir, "objects"))
	if err != nil || !objs.IsDir() {
		return false
	}
	refs, err := os.Stat(filepath.Join(dir, "refs"))
	if err != nil || !refs.IsDir() {
		return false
	}
	return true
}

// localPath maps source to <destDir>/<host>/<path>, stripping a trailing
// ".git" from the path so that https://host/a/b and https://host/a/b.git and
// git@host:a/b.git all resolve to the same local directory.
func localPath(destDir, source string) (string, error) {
	host, repoPath, err := parseSource(source)
	if err != nil {
		return "", err
	}
	return filepath.Join(destDir, host, filepath.FromSlash(repoPath)), nil
}

func parseSource(source string) (host, repoPath string, err error) {
	if strings.Contains(source, "://") {
		u, err := url.Parse(source)
		if err != nil {
			return "", "", fmt.Errorf("parsing %q: %w", source, err)
		}
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "http", "https", "git", "ssh", "git+ssh":
		default:
			return "", "", fmt.Errorf("unsupported git URL scheme %q in %q", u.Scheme, source)
		}
		host = strings.ToLower(u.Host)
		for _, def := range []struct{ scheme, suffix string }{
			{"http", ":80"},
			{"https", ":443"},
			{"git", ":9418"},
			{"ssh", ":22"},
			{"git+ssh", ":22"},
		} {
			if scheme == def.scheme && strings.HasSuffix(host, def.suffix) {
				host = host[:len(host)-len(def.suffix)]
			}
		}
		return cleanHostAndPath(source, host, u.Path)
	}

	if strings.Contains(source, "::") {
		return "", "", fmt.Errorf("not a git source: %q", source)
	}
	colon := strings.IndexByte(source, ':')
	if colon <= 0 {
		return "", "", fmt.Errorf("not a git source (want a URL or [user@]host:path): %q", source)
	}
	hostPart := source[:colon]
	if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	return cleanHostAndPath(source, strings.ToLower(hostPart), source[colon+1:])
}

func cleanHostAndPath(source, host, rawPath string) (string, string, error) {
	if host == "" || host == "." || host == ".." || strings.ContainsAny(host, `/\`) {
		return "", "", fmt.Errorf("unusable host %q in %q", host, source)
	}
	p := strings.TrimPrefix(rawPath, "/")
	p = strings.TrimPrefix(p, "~/")
	p = strings.TrimPrefix(p, "~")
	p = path.Clean("/" + p)
	p = strings.TrimSuffix(p, ".git")
	if p == "" || p == "/" || p == "." {
		return "", "", fmt.Errorf("no repository path in %q", source)
	}
	if strings.HasSuffix(p, tmpSuffix) {
		return "", "", fmt.Errorf("path %q collides with temp directory naming", rawPath)
	}
	return host, strings.TrimPrefix(p, "/"), nil
}
