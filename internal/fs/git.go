package fs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zyx1121/kitbash/internal/problem"
)

// commitFormat feeds git log. ASCII unit and record separators delimit the
// fields so a commit message may contain newlines.
const (
	unitSep      = "\x1f"
	recordSep    = "\x1e"
	commitFormat = "%H%x1f%an%x1f%aI%x1f%B%x1e"
)

// Commit is one entry of a folder's history.
type Commit struct {
	Sha     string `json:"sha"`
	Author  string `json:"author"`
	Time    string `json:"time"`
	Message string `json:"message"`
}

// memberName is the shape of a member's Linux name, the same pattern the users
// family publishes. An author reaches git as one argument of a commit, so the
// name is held to it rather than trusted.
var memberName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// authorship is who a commit belongs to when the caller is not the author. An
// approved call is committed by the admin whose session ran it and authored by
// the member who asked for it, with a trailer naming the admin, see PLAN.md
// section 2.1. The zero value is the ordinary case: the caller is the author.
type authorship struct {
	Author     string
	ApprovedBy string
}

// check refuses an authorship kitbash would not put in a commit.
func (a authorship) check(instance string) *problem.Problem {
	if a.Author == "" && a.ApprovedBy == "" {
		return nil
	}
	if !memberName.MatchString(a.Author) {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a member name, so the commit has no author", a.Author),
			"Author a commit as the member who asked for the write.")
	}
	if a.ApprovedBy != "" && !memberName.MatchString(a.ApprovedBy) {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a member name, so the commit has no approver", a.ApprovedBy),
			"Name the admin who approved the write.")
	}
	return nil
}

// message appends the Approved-by trailer, which is how a commit says who let
// it into the shared root.
func (a authorship) message(message string) string {
	if a.ApprovedBy == "" {
		return message
	}
	return message + "\n\nApproved-by: " + a.ApprovedBy
}

// args are what git commit is given to attribute the commit to the author.
// The committer is left to the environment, which is the session's own user.
func (a authorship) args() []string {
	if a.Author == "" {
		return nil
	}
	return []string{"--author", fmt.Sprintf("%s <%s@kitbash>", a.Author, a.Author)}
}

// isPermissionDenied reports whether a git failure was the operating system
// refusing the write, which is what a member writing into /org gets.
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "read-only file system") ||
		strings.Contains(msg, "operation not permitted")
}

// isRepo reports whether dir is the top of a git repository. Lstat, not Stat:
// a .git that is a symlink is not a repository as far as kitbash is concerned,
// because kitbash follows no symlinks.
func isRepo(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

// git runs one git command inside repo and returns its standard output.
//
// Its error carries git's standard error for the server log only. Never put
// that message in a problem detail: map it with gitProblem, which sends it to
// problem.Internal, so the agent gets a generic detail and the operator gets
// the cause.
func (s *Service) git(ctx context.Context, repo string, args ...string) (string, error) {
	// /org and its folders are owned by root, so a member's git refuses to
	// operate on them unless the repository is declared safe.
	full := append([]string{
		"-c", "safe.directory=" + repo,
		"-c", "commit.gpgsign=false",
		"-c", "advice.detachedHead=false",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+s.user,
		"GIT_AUTHOR_EMAIL="+s.user+"@kitbash",
		"GIT_COMMITTER_NAME="+s.user,
		"GIT_COMMITTER_EMAIL="+s.user+"@kitbash",
		"GIT_TERMINAL_PROMPT=0",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}

// history reads the commits that touched rel inside repo, newest first.
func (s *Service) history(ctx context.Context, repo, rel string, limit int) ([]Commit, error) {
	if !isRepo(repo) {
		return nil, nil
	}
	args := []string{"log", "--format=" + commitFormat, fmt.Sprintf("-n%d", limit)}
	if rel != "" {
		args = append(args, "--", rel)
	}
	out, err := s.git(ctx, repo, args...)
	if err != nil {
		// A repository without commits is not an error, it is an empty history.
		if strings.Contains(err.Error(), "does not have any commits yet") ||
			strings.Contains(err.Error(), "unknown revision") {
			return nil, nil
		}
		return nil, err
	}
	return parseCommits(out), nil
}

// parseCommits reads the NUL delimited output of git log.
func parseCommits(out string) []Commit {
	var commits []Commit
	for _, record := range strings.Split(out, recordSep) {
		record = strings.Trim(record, "\n")
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, unitSep, 4)
		if len(parts) < 4 {
			continue
		}
		commits = append(commits, Commit{
			Sha:     strings.TrimSpace(parts[0]),
			Author:  parts[1],
			Time:    parts[2],
			Message: strings.TrimSpace(parts[3]),
		})
	}
	return commits
}

// lastCommit returns the newest commit that touched rel, or nil when there is
// none.
func (s *Service) lastCommit(ctx context.Context, repo, rel string) (*Commit, error) {
	commits, err := s.history(ctx, repo, rel, 1)
	if err != nil || len(commits) == 0 {
		return nil, err
	}
	return &commits[0], nil
}
