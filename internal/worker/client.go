// Package worker is the other side of a networked run: a machine that joins a primary with a token, pins its
// certificate, then takes shards from it, runs them in its own workspace, database and stack, and sends the report,
// the log and the artifacts back. Everything goes to the primary's worker listener over TLS; nothing here listens
// beyond 127.0.0.1.
package worker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/server"
)

// ErrRevoked is returned when the primary no longer accepts this worker's credential.
var ErrRevoked = errors.New("the primary refused this worker's credential (revoked): join again with a new token")

// Config is what a joined worker keeps in its data directory (mode 0600).
type Config struct {
	URL         string `json:"url"` // https://host:port of the primary's worker listener
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	Credential  string `json:"credential"`
}

const configFile = "worker.json"

// SaveConfig writes the worker's join result.
func SaveConfig(dataDir string, c Config) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(dataDir, configFile), b, 0o600)
}

// LoadConfig reads it back.
func LoadConfig(dataDir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dataDir, configFile))
	if err != nil {
		return c, fmt.Errorf("this machine has not joined a primary yet (run `localci worker join`): %w", err)
	}
	return c, json.Unmarshal(b, &c)
}

// PinnedClient trusts exactly one certificate: the one whose SHA-256 fingerprint is fp. The primary's certificate is
// self-signed, so no name or chain is checked; a different certificate fails the handshake.
func PinnedClient(fp string) *http.Client {
	want := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the primary sent no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("the primary's certificate does not match the fingerprint you pinned")
			}
			return nil
		},
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ResponseHeaderTimeout: 2 * time.Minute, MaxIdleConnsPerHost: 8}}
}

// Join redeems a join token at the primary and returns the worker's config, to be saved.
func Join(ctx context.Context, url, token, fingerprint, name string, capacity int) (Config, error) {
	url = strings.TrimRight(url, "/")
	body, _ := json.Marshal(server.JoinRequest{Token: token, Name: name, Capacity: capacity})
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/worker/v1/join", strings.NewReader(string(body)))
	resp, err := PinnedClient(fingerprint).Do(req)
	if err != nil {
		return Config{}, fmt.Errorf("join %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("join: %s", errorText(resp))
	}
	var jr server.JoinResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil || jr.Credential == "" {
		return Config{}, errors.New("join: the primary's answer was not understood")
	}
	return Config{URL: url, Fingerprint: fingerprint, Name: name, Credential: jr.Credential}, nil
}

func errorText(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e server.Error
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Sprintf("%s (%d)", e.Error, resp.StatusCode)
	}
	return fmt.Sprintf("HTTP %d", resp.StatusCode)
}
