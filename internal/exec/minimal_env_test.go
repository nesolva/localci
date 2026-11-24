package exec

import (
	"os"
	"strings"
	"testing"
)

func TestAMinimalEnvProcessSeesNoSecretsOfTheDaemonButKeepsWhatItNeeds(t *testing.T) {
	for k, v := range map[string]string{
		"GITHUB_TOKEN": "ghp_secret", "AWS_SECRET_ACCESS_KEY": "aws_secret", "LOCALCI_ADMIN": "adm",
		"HTTPS_PROXY": "http://proxy.invalid:3128", "ANTHROPIC_API_KEY": "ak", "CLAUDE_CONFIG_DIR": "/cfg",
		"LC_TIME": "C", "LANG": "C", "SSL_CERT_FILE": "/ca.pem", "NODE_EXTRA_CA_CERTS": "/ca.pem",
		"SHELL": "/bin/zsh", "XDG_CONFIG_HOME": "/xdg",
	} {
		t.Setenv(k, v)
	}
	lp := logPath(t)
	p, err := Start(bg, Spec{Cmd: `env`, Dir: t.TempDir(), LogPath: lp, MinimalEnv: true, Env: []string{"LOCALCI_FINDINGS=/f"}})
	if err != nil {
		t.Fatal(err)
	}
	if res := p.Wait(); !res.OK() {
		t.Fatalf("exit %d", res.ExitCode)
	}
	b, _ := os.ReadFile(lp)
	out := string(b)
	for _, leaked := range []string{"GITHUB_TOKEN", "ghp_secret", "AWS_SECRET_ACCESS_KEY", "LOCALCI_ADMIN"} {
		if strings.Contains(out, leaked) {
			t.Errorf("the agent's environment leaks %s:\n%s", leaked, out)
		}
	}
	for _, kept := range []string{"PATH=", "HOME=", "HTTPS_PROXY=http://proxy.invalid:3128", "ANTHROPIC_API_KEY=ak", "CLAUDE_CONFIG_DIR=/cfg", "LC_TIME=C", "LOCALCI_FINDINGS=/f",
		"SSL_CERT_FILE=/ca.pem", "NODE_EXTRA_CA_CERTS=/ca.pem", "SHELL=/bin/zsh", "XDG_CONFIG_HOME=/xdg"} {
		if !strings.Contains(out, kept) {
			t.Errorf("the agent's environment lost %s:\n%s", kept, out)
		}
	}
}
