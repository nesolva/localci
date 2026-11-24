package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/suite"
)

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if origin != "" {
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", origin).CombinedOutput(); err != nil {
			t.Fatalf("remote add: %v: %s", err, out)
		}
	}
	return dir
}

func TestCheckoutNamesTheRepoWhateverTheRemoteHostIs(t *testing.T) {
	for _, url := range []string{
		"https://github.com/acme/web.git",
		"git@github.com:acme/web.git",
		"git@github-work:acme/web.git", // an SSH host alias
		"ssh://git@gh.internal.example/acme/web",
		"https://github.com/acme/web/",
	} {
		t.Run(url, func(t *testing.T) {
			co, err := checkoutOf(context.Background(), gitRepo(t, url))
			if err != nil || co.repo != "acme/web" {
				t.Fatalf("repo = %q, err = %v; want acme/web", co.repo, err)
			}
		})
	}
}

func TestCheckoutReadsTheConfiguredURLNotTheRewrittenOne(t *testing.T) {
	dir := gitRepo(t, "gh:acme/web.git")
	if out, err := exec.Command("git", "-C", dir, "config", "url.https://example.invalid/mirror/.insteadOf", "gh:").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	co, err := checkoutOf(context.Background(), dir)
	if err != nil || co.repo != "acme/web" {
		t.Fatalf("repo = %q, err = %v; want acme/web", co.repo, err)
	}
}

func TestSuiteHeadsRefuseADirectoryOutsideTheSuite(t *testing.T) {
	sd := &suite.Suite{Name: "e2e", Repos: []string{"acme/web", "acme/api"}}
	h, err := suiteHeads(context.Background(), sd, gitRepo(t, "git@github-work:acme/web.git"))
	if err != nil || len(h) != 1 || h[0].Repo != "acme/web" || h[0].Dir == "" {
		t.Fatalf("heads = %+v, err = %v", h, err)
	}
	_, err = suiteHeads(context.Background(), sd, gitRepo(t, "git@github.com:acme/other.git"))
	if err == nil || !strings.Contains(err.Error(), "acme/other") || !strings.Contains(err.Error(), "acme/web") {
		t.Fatalf("err = %v; want one naming the repo and the suite's repos", err)
	}
	if _, err = suiteHeads(context.Background(), sd, t.TempDir()); err == nil {
		t.Fatal("a directory that is not a git checkout must be refused")
	}
}
