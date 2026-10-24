package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// releaseBase is where tagged release assets live. A package var so tests
// can point it at a local server.
var releaseBase = "https://github.com/" + repo + "/releases/download"

const (
	checksumsAsset = "checksums.txt"
	// signatureAsset is cosign's keyless Sigstore bundle over checksums.txt
	// (see signs: in .goreleaser.yml).
	signatureAsset = "checksums.txt.sigstore.json"
	signerIdentity = `^https://github\.com/polius/fsend/\.github/workflows/release\.yml@refs/tags/v`
	signerIssuer   = "https://token.actions.githubusercontent.com"

	maxAssetBytes   = 4 << 20
	downloadTimeout = 60 * time.Second
)

// lookCosign finds cosign on PATH; a var so tests can stub it.
var lookCosign = func() (string, error) { return exec.LookPath("cosign") }

// downloadClient refuses redirects that change scheme, so an https asset
// can never be served over plain http.
var downloadClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != via[0].URL.Scheme {
			return fmt.Errorf("refusing redirect from %s to %s", via[0].URL.Scheme, req.URL.Scheme)
		}
		return nil
	},
}

// FetchInstaller downloads the install script name published with release
// version into dir and verifies it against that release's checksums.txt.
// When cosign is on PATH, checksums.txt must first carry a valid signature
// from this repo's release workflow; signed reports whether that ran. A
// release without a signature is then an error, not a silent downgrade.
func FetchInstaller(ctx context.Context, version, name, dir string) (path string, signed bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	tag := "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")

	sums, err := fetchAsset(ctx, tag, checksumsAsset)
	if err != nil {
		return "", false, err
	}
	if cosign, lerr := lookCosign(); lerr == nil {
		if err := verifySignature(ctx, cosign, tag, sums, dir); err != nil {
			return "", false, err
		}
		signed = true
	}

	want, ok := checksumFor(sums, name)
	if !ok {
		return "", false, fmt.Errorf("release %s publishes no checksum for %s", tag, name)
	}
	script, err := fetchAsset(ctx, tag, name)
	if err != nil {
		return "", false, err
	}
	got := sha256.Sum256(script)
	if hex.EncodeToString(got[:]) != want {
		return "", false, fmt.Errorf("checksum mismatch for %s", name)
	}
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, script, 0o600); err != nil {
		return "", false, err
	}
	return path, signed, nil
}

// verifySignature checks cosign's bundle over sums against the release
// workflow's identity.
func verifySignature(ctx context.Context, cosign, tag string, sums []byte, dir string) error {
	bundle, err := fetchAsset(ctx, tag, signatureAsset)
	if err != nil {
		return fmt.Errorf("cosign is installed but the release signature is unavailable: %w", err)
	}
	sumsPath := filepath.Join(dir, checksumsAsset)
	bundlePath := filepath.Join(dir, signatureAsset)
	if err := os.WriteFile(sumsPath, sums, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, cosign, "verify-blob", //nolint:gosec // G204: cosign from PATH, fixed args
		"--bundle", bundlePath,
		"--certificate-identity-regexp", signerIdentity,
		"--certificate-oidc-issuer", signerIssuer,
		sumsPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("release signature check failed: %v: %s", err, lastLine(out))
	}
	return nil
}

// checksumFor finds name's hex digest in a sha256sum-format file.
func checksumFor(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

func fetchAsset(ctx context.Context, tag, name string) ([]byte, error) {
	url := releaseBase + "/" + tag + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fsend-update")
	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", name, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	if len(data) > maxAssetBytes {
		return nil, fmt.Errorf("downloading %s: larger than %d bytes", name, maxAssetBytes)
	}
	return data, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
